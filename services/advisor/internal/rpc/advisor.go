// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package rpc serves the AdvisorService Connect API. It is strictly
// read-only: briefings and proposed actions out, nothing in. A small
// in-memory cache (10 min TTL) keeps repeated dashboard loads from
// re-billing the LLM tier.
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
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// DefaultTTL is how long a generated briefing is served from cache.
const DefaultTTL = 10 * time.Minute

// Advisor implements kuberov1connect.AdvisorServiceHandler.
type Advisor struct {
	Brain  brain.Brain
	Source source.Source
	Log    *slog.Logger

	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// TTL for the briefing cache; defaults to DefaultTTL.
	TTL time.Duration

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	briefing *kuberov1.Briefing
	expires  time.Time
}

// New wires the handler.
func New(b brain.Brain, src source.Source, log *slog.Logger) *Advisor {
	return &Advisor{
		Brain:  b,
		Source: src,
		Log:    log,
		Now:    time.Now,
		TTL:    DefaultTTL,
		cache:  map[string]cacheEntry{},
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
	now := a.Now()

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

// Investigate lands with the investigation agent; until then it
// reports Unimplemented rather than guessing.
func (a *Advisor) Investigate(
	_ context.Context,
	_ *connect.Request[kuberov1.InvestigateRequest],
) (*connect.Response[kuberov1.InvestigateResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("investigate: not available in this build"))
}

// InvestigateStream is the streaming form of Investigate.
func (a *Advisor) InvestigateStream(
	_ context.Context,
	_ *connect.Request[kuberov1.InvestigateStreamRequest],
	_ *connect.ServerStream[kuberov1.InvestigateStreamResponse],
) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("investigate: not available in this build"))
}
