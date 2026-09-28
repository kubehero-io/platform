// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package rpc serves the AdvisorService Connect API. It is strictly
// read-only: briefings, investigations and proposed actions out, nothing
// in. Small in-memory caches (briefings 10 min, investigations 1 min per
// question) keep repeated dashboard loads from re-billing the LLM tier.
package rpc

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/investigate"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// DefaultTTL is how long a generated briefing is served from cache.
const DefaultTTL = 10 * time.Minute

// DefaultInvestigateTTL is how long an investigation answer is reused
// for the identical question (double-clicks, several viewers).
const DefaultInvestigateTTL = time.Minute

// maxInvestigateCache bounds the investigation cache.
const maxInvestigateCache = 256

// Advisor implements kuberov1connect.AdvisorServiceHandler.
type Advisor struct {
	Brain  brain.Brain
	Source source.Source
	Log    *slog.Logger

	// Investigator answers Investigate / InvestigateStream; nil reports
	// Unimplemented.
	Investigator investigate.Investigator
	// DemoData labels investigation answers source="demo" (the advisor
	// runs over built-in fixtures).
	DemoData bool

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// TTL for the briefing cache; defaults to DefaultTTL.
	TTL time.Duration
	// InvestigateTTL for the investigation cache; defaults to DefaultInvestigateTTL.
	InvestigateTTL time.Duration

	mu       sync.Mutex
	cache    map[string]cacheEntry
	invCache map[string]invCacheEntry
}

type cacheEntry struct {
	briefing *kuberov1.Briefing
	expires  time.Time
}

type invCacheEntry struct {
	res     *kuberov1.InvestigateResponse
	expires time.Time
}

// New wires the handler.
func New(b brain.Brain, src source.Source, log *slog.Logger) *Advisor {
	return &Advisor{
		Brain:          b,
		Source:         src,
		Log:            log,
		Now:            time.Now,
		TTL:            DefaultTTL,
		InvestigateTTL: DefaultInvestigateTTL,
		cache:          map[string]cacheEntry{},
		invCache:       map[string]invCacheEntry{},
	}
}

// Compile-time assertion the server matches the generated interface.
var _ kuberov1connect.AdvisorServiceHandler = (*Advisor)(nil)

func (a *Advisor) GetBriefing(
	ctx context.Context,
	req *connect.Request[kuberov1.GetBriefingRequest],
) (*connect.Response[kuberov1.GetBriefingResponse], error) {
	window := normalizeWindow(req.Msg.GetWindow())
	briefing, err := a.briefing(ctx, req.Msg.GetClusterId(), window)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kuberov1.GetBriefingResponse{Briefing: briefing}), nil
}

func (a *Advisor) ListAdvice(
	ctx context.Context,
	req *connect.Request[kuberov1.ListAdviceRequest],
) (*connect.Response[kuberov1.ListAdviceResponse], error) {
	briefing, err := a.briefing(ctx, req.Msg.GetClusterId(), "24h")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kuberov1.ListAdviceResponse{Actions: briefing.GetActions()}), nil
}

// briefing returns a cached briefing or generates a fresh one.
func (a *Advisor) briefing(ctx context.Context, clusterID, window string) (*kuberov1.Briefing, error) {
	key := clusterID + "|" + window
	now := a.now()

	a.mu.Lock()
	if e, ok := a.cache[key]; ok && now.Before(e.expires) {
		a.mu.Unlock()
		// Clone so callers can't mutate the cached copy.
		return proto.Clone(e.briefing).(*kuberov1.Briefing), nil
	}
	a.mu.Unlock()

	snap, err := a.Source.Fetch(ctx, clusterID, window)
	if err != nil {
		// Degrade rather than fail: the advisor must keep answering even
		// when the control-plane is unreachable (mirrors its stub mode).
		a.Log.Warn("source fetch failed, using demo snapshot", "err", err)
		snap, _ = source.Demo{}.Fetch(ctx, clusterID, window)
	}

	briefing, err := a.Brain.Generate(ctx, snap)
	if err != nil {
		return nil, err
	}
	briefing.Id = "brf-" + randomHex(4)
	briefing.GeneratedAtUnix = now.Unix()
	if snap.Origin == "demo" {
		briefing.Source = "demo"
	}
	for _, act := range briefing.Actions {
		act.Status = brain.StatusProposed
	}

	a.mu.Lock()
	a.cache[key] = cacheEntry{briefing: briefing, expires: now.Add(a.ttl())}
	a.mu.Unlock()
	return proto.Clone(briefing).(*kuberov1.Briefing), nil
}

func (a *Advisor) ttl() time.Duration {
	if a.TTL > 0 {
		return a.TTL
	}
	return DefaultTTL
}

func normalizeWindow(w string) string {
	switch w {
	case "7d":
		return "7d"
	default:
		return "24h"
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "00000000"[:n*2]
	}
	return hex.EncodeToString(b)
}

// Investigate answers a free-form question with a read-only tool loop
// (LLM when configured, deterministic rules otherwise).
func (a *Advisor) Investigate(
	ctx context.Context,
	req *connect.Request[kuberov1.InvestigateRequest],
) (*connect.Response[kuberov1.InvestigateResponse], error) {
	r, err := a.investigateRequest(req.Msg)
	if err != nil {
		return nil, err
	}
	if cached := a.cachedInvestigation(r.Key()); cached != nil {
		return connect.NewResponse(cached), nil
	}
	res, err := a.Investigator.Investigate(ctx, r, nil)
	if err != nil {
		return nil, investigateError(ctx, err)
	}
	return connect.NewResponse(a.storeInvestigation(r.Key(), res)), nil
}

// InvestigateStream is Investigate with live progress: every tool call
// arrives as a step event while it happens, then the final result.
func (a *Advisor) InvestigateStream(
	ctx context.Context,
	req *connect.Request[kuberov1.InvestigateStreamRequest],
	stream *connect.ServerStream[kuberov1.InvestigateStreamResponse],
) error {
	r, err := a.investigateRequest(req.Msg.GetRequest())
	if err != nil {
		return err
	}
	if cached := a.cachedInvestigation(r.Key()); cached != nil {
		if err := stream.Send(progressEvent("Answer from the last minute's identical question")); err != nil {
			return err
		}
		return stream.Send(&kuberov1.InvestigateStreamResponse{Event: &kuberov1.InvestigateStreamResponse_Result{Result: cached}})
	}

	// Tools run concurrently; connect streams are not safe for concurrent
	// Send, so every event funnels through one lock. A failed send (the
	// client went away) cancels the investigation.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mu sync.Mutex
	var sendErr error
	emit := func(ev *kuberov1.InvestigateStreamResponse) {
		mu.Lock()
		defer mu.Unlock()
		if sendErr != nil {
			return
		}
		if err := stream.Send(ev); err != nil {
			sendErr = err
			cancel()
		}
	}
	res, err := a.Investigator.Investigate(ctx, r, emit)
	mu.Lock()
	defer mu.Unlock()
	if sendErr != nil {
		return sendErr
	}
	if err != nil {
		return investigateError(ctx, err)
	}
	res = a.storeInvestigation(r.Key(), res)
	return stream.Send(&kuberov1.InvestigateStreamResponse{Event: &kuberov1.InvestigateStreamResponse_Result{Result: res}})
}

func (a *Advisor) investigateRequest(msg *kuberov1.InvestigateRequest) (investigate.Request, error) {
	if a.Investigator == nil {
		return investigate.Request{}, connect.NewError(connect.CodeUnimplemented, errors.New("investigate: no investigator configured"))
	}
	if msg == nil {
		msg = &kuberov1.InvestigateRequest{}
	}
	r, err := investigate.NormalizeRequest(msg)
	if err != nil {
		return investigate.Request{}, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return r, nil
}

func investigateError(ctx context.Context, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(ctx.Err(), context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func progressEvent(msg string) *kuberov1.InvestigateStreamResponse {
	return &kuberov1.InvestigateStreamResponse{Event: &kuberov1.InvestigateStreamResponse_Progress{Progress: msg}}
}

func (a *Advisor) cachedInvestigation(key string) *kuberov1.InvestigateResponse {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e, ok := a.invCache[key]; ok && a.now().Before(e.expires) {
		return proto.Clone(e.res).(*kuberov1.InvestigateResponse)
	}
	return nil
}

// storeInvestigation stamps id + time (+ the demo label), caches the
// answer and returns a copy safe to hand out.
func (a *Advisor) storeInvestigation(key string, res *kuberov1.InvestigateResponse) *kuberov1.InvestigateResponse {
	now := a.now()
	res.Id = investigate.NewID()
	res.GeneratedAtUnix = now.Unix()
	if a.DemoData {
		res.Source = "demo"
	}
	for _, act := range res.GetActions() {
		act.Status = brain.StatusProposed
	}
	ttl := a.InvestigateTTL
	if ttl <= 0 {
		ttl = DefaultInvestigateTTL
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.invCache == nil {
		a.invCache = map[string]invCacheEntry{}
	}
	if len(a.invCache) >= maxInvestigateCache {
		for k, e := range a.invCache {
			if !now.Before(e.expires) {
				delete(a.invCache, k)
			}
		}
		for k := range a.invCache {
			if len(a.invCache) < maxInvestigateCache {
				break
			}
			delete(a.invCache, k)
		}
	}
	a.invCache[key] = invCacheEntry{res: res, expires: now.Add(ttl)}
	return proto.Clone(res).(*kuberov1.InvestigateResponse)
}

func (a *Advisor) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}
