// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package callerauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// fakeCP answers WhoAmI from a token → principal table. An empty
// Authorization header gets anon (nil: the control plane requires auth).
type fakeCP struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	tokens map[string]*kuberov1.WhoAmIResponse
	anon   *kuberov1.WhoAmIResponse
	calls  atomic.Int64
}

func (f *fakeCP) WhoAmI(_ context.Context, req *connect.Request[kuberov1.WhoAmIRequest]) (*connect.Response[kuberov1.WhoAmIResponse], error) {
	f.calls.Add(1)
	h := req.Header().Get("Authorization")
	if h == "" {
		if f.anon == nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing Authorization header"))
		}
		return connect.NewResponse(f.anon), nil
	}
	if who, ok := f.tokens[h]; ok {
		return connect.NewResponse(who), nil
	}
	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
}

func newVerifier(t *testing.T, cp *fakeCP) (*Verifier, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(cp))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client(), nil), srv
}

func secured() *fakeCP {
	return &fakeCP{tokens: map[string]*kuberov1.WhoAmIResponse{
		"Bearer viewer":  {Subject: "key:v", Role: "viewer", AuthRequired: true},
		"Bearer auditor": {Subject: "key:a", Role: "auditor", AuthRequired: true},
		"Bearer anon":    {Subject: "anonymous", Role: "anonymous", AuthRequired: true},
		"Bearer cluster": {Subject: "cluster:c1", Role: "member", ClusterId: "c1", AuthRequired: true},
	}}
}

func TestCheckVerdicts(t *testing.T) {
	v, _ := newVerifier(t, secured())
	ctx := context.Background()
	for _, tc := range []struct {
		name, header string
		code         connect.Code // 0: allowed
	}{
		{"viewer token", "Bearer viewer", 0},
		{"auditor token", "Bearer auditor", 0},
		{"no token on a secured control plane", "", connect.CodeUnauthenticated},
		{"unknown token", "Bearer nope", connect.CodeUnauthenticated},
		{"anonymous role", "Bearer anon", connect.CodePermissionDenied},
		{"cluster enrollment token", "Bearer cluster", connect.CodePermissionDenied},
	} {
		err := v.Check(ctx, tc.header)
		if tc.code == 0 {
			if err != nil {
				t.Errorf("%s: refused: %v", tc.name, err)
			}
			continue
		}
		if connect.CodeOf(err) != tc.code {
			t.Errorf("%s: code = %v (%v), want %v", tc.name, connect.CodeOf(err), err, tc.code)
		}
	}
}

func TestAnonymousAllowedOnlyWhenControlPlaneIsOpen(t *testing.T) {
	cp := secured()
	cp.anon = &kuberov1.WhoAmIResponse{Subject: "anonymous", Role: "admin"} // auth off: anonymous is admin
	v, _ := newVerifier(t, cp)
	if err := v.Check(context.Background(), ""); err != nil {
		t.Fatalf("open control plane: anonymous refused: %v", err)
	}
}

func TestVerdictsAreCached(t *testing.T) {
	cp := secured()
	v, _ := newVerifier(t, cp)
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	v.now = func() time.Time { return now }
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_ = v.Check(ctx, "Bearer viewer")
		_ = v.Check(ctx, "Bearer nope")
	}
	if got := cp.calls.Load(); got != 2 {
		t.Fatalf("WhoAmI calls = %d, want 2 (one per token)", got)
	}
	now = now.Add(defaultNegTTL + time.Second) // rejection expires first
	_ = v.Check(ctx, "Bearer nope")
	_ = v.Check(ctx, "Bearer viewer")
	if got := cp.calls.Load(); got != 3 {
		t.Fatalf("WhoAmI calls = %d, want 3 (only the rejected token re-checked)", got)
	}
	now = now.Add(defaultTTL) // and then the approval
	_ = v.Check(ctx, "Bearer viewer")
	if got := cp.calls.Load(); got != 4 {
		t.Fatalf("WhoAmI calls = %d, want 4", got)
	}
}

func TestControlPlaneDownFailsClosedAndIsNotCached(t *testing.T) {
	cp := secured()
	v, srv := newVerifier(t, cp)
	srv.Close()
	for i := 0; i < 2; i++ {
		if err := v.Check(context.Background(), "Bearer viewer"); connect.CodeOf(err) != connect.CodeUnavailable {
			t.Fatalf("call %d: err = %v, want Unavailable", i, err)
		}
	}
	if len(v.cache) != 0 {
		t.Fatalf("an outage verdict was cached: %v", v.cache)
	}
}

// The interceptor guards unary and server-streaming RPCs alike; allowed
// calls reach the handler (here: Unimplemented).
func TestInterceptorGuardsUnaryAndStreaming(t *testing.T) {
	v, _ := newVerifier(t, secured())
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewAdvisorServiceHandler(kuberov1connect.UnimplementedAdvisorServiceHandler{},
		connect.WithInterceptors(v.Interceptor())))
	adv := httptest.NewServer(mux)
	t.Cleanup(adv.Close)
	client := kuberov1connect.NewAdvisorServiceClient(adv.Client(), adv.URL)
	ctx := context.Background()

	brief := func(header string) error {
		req := connect.NewRequest(&kuberov1.GetBriefingRequest{})
		if header != "" {
			req.Header().Set("Authorization", header)
		}
		_, err := client.GetBriefing(ctx, req)
		return err
	}
	stream := func(header string) error {
		req := connect.NewRequest(&kuberov1.InvestigateStreamRequest{Request: &kuberov1.InvestigateRequest{Question: "why?"}})
		if header != "" {
			req.Header().Set("Authorization", header)
		}
		s, err := client.InvestigateStream(ctx, req)
		if err != nil {
			return err
		}
		for s.Receive() {
		}
		return s.Err()
	}
	for name, call := range map[string]func(string) error{"unary": brief, "stream": stream} {
		if err := call(""); connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Errorf("%s without a token: %v, want Unauthenticated", name, err)
		}
		if err := call("Bearer viewer"); connect.CodeOf(err) != connect.CodeUnimplemented {
			t.Errorf("%s with a viewer token: %v, want it to reach the handler", name, err)
		}
	}
}
