// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package callerauth authenticates the advisor's callers against the
// control plane.
//
// The advisor reads the control plane with its own service token, so
// without a check of its own any pod that can reach it would get what the
// control plane only shows to authenticated users — and could spend the
// operator's Anthropic budget. Every RPC's bearer token is verified with
// ControlPlaneService.WhoAmI and must carry at least the viewer role;
// results are cached briefly. A caller without a token passes only when
// the control plane itself accepts anonymous requests (a dev install with
// auth off), so the advisor is never more open than the control plane.
package callerauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

const (
	defaultTTL     = time.Minute      // how long a verified caller is trusted
	defaultNegTTL  = 10 * time.Second // how long a rejected token stays rejected
	defaultTimeout = 5 * time.Second  // one WhoAmI round trip
	maxEntries     = 4096
)

// Roles that may use the advisor: viewer and everything that satisfies it
// on the control plane (auditor sits beside member there).
var allowed = map[string]bool{"viewer": true, "member": true, "auditor": true, "admin": true, "owner": true}

// Verifier checks callers with the control plane's WhoAmI.
type Verifier struct {
	client kuberov1connect.ControlPlaneServiceClient
	log    *slog.Logger
	ttl    time.Duration
	negTTL time.Duration
	now    func() time.Time

	mu    sync.Mutex
	cache map[[32]byte]entry
}

type entry struct {
	err     error // nil: allowed
	expires time.Time
}

// New returns a Verifier for the control plane at baseURL. httpClient may
// be nil (a client with a short timeout is used).
func New(baseURL string, httpClient connect.HTTPClient, log *slog.Logger) *Verifier {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 2 * defaultTimeout}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Verifier{
		client: kuberov1connect.NewControlPlaneServiceClient(httpClient, baseURL),
		log:    log,
		ttl:    defaultTTL,
		negTTL: defaultNegTTL,
		now:    time.Now,
		cache:  map[[32]byte]entry{},
	}
}

// Check authorizes one call carrying the given Authorization header value
// ("" for none). It returns a Connect error when the caller is refused.
func (v *Verifier) Check(ctx context.Context, authorization string) error {
	key := sha256.Sum256([]byte(authorization))
	now := v.now()
	v.mu.Lock()
	if e, ok := v.cache[key]; ok && now.Before(e.expires) {
		v.mu.Unlock()
		return e.err
	}
	v.mu.Unlock()

	cacheable, err := v.verify(ctx, authorization)
	if cacheable {
		ttl := v.ttl
		if err != nil {
			ttl = v.negTTL
		}
		v.mu.Lock()
		if len(v.cache) >= maxEntries {
			v.evictExpired(now)
		}
		if len(v.cache) < maxEntries {
			v.cache[key] = entry{err: err, expires: now.Add(ttl)}
		}
		v.mu.Unlock()
	}
	return err
}

// verify asks the control plane who the caller is. cacheable is false for
// transport failures: those must not pin a verdict.
func (v *Verifier) verify(ctx context.Context, authorization string) (cacheable bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req := connect.NewRequest(&kuberov1.WhoAmIRequest{})
	if authorization != "" {
		req.Header().Set("Authorization", authorization)
	}
	res, callErr := v.client.WhoAmI(ctx, req)
	if callErr != nil {
		switch connect.CodeOf(callErr) {
		case connect.CodeUnauthenticated:
			if authorization == "" {
				return true, connect.NewError(connect.CodeUnauthenticated,
					errors.New("the advisor needs a KubeHero access token (the same one the control plane accepts)"))
			}
			return true, connect.NewError(connect.CodeUnauthenticated, errors.New("the control plane rejected this token"))
		case connect.CodePermissionDenied:
			return true, connect.NewError(connect.CodePermissionDenied, errors.New("this token may not read from KubeHero"))
		}
		v.log.Warn("advisor could not verify a caller; refusing the call", "err", callErr)
		return false, connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("can't verify the caller with the control plane: %w", callErr))
	}
	who := res.Msg
	if who.GetClusterId() != "" {
		return true, connect.NewError(connect.CodePermissionDenied,
			errors.New("cluster enrollment tokens can't use the advisor; use a user or API token"))
	}
	if !allowed[who.GetRole()] {
		return true, connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("role %q may not use the advisor (viewer or above required)", who.GetRole()))
	}
	return true, nil
}

func (v *Verifier) evictExpired(now time.Time) {
	for k, e := range v.cache {
		if !now.Before(e.expires) {
			delete(v.cache, k)
		}
	}
}

// Interceptor enforces Check on every unary and streaming RPC it wraps.
func (v *Verifier) Interceptor() connect.Interceptor { return interceptor{v} }

type interceptor struct{ v *Verifier }

func (i interceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := i.v.Check(ctx, req.Header().Get("Authorization")); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (i interceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i interceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := i.v.Check(ctx, conn.RequestHeader().Get("Authorization")); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
