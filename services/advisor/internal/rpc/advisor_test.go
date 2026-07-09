// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/advisor/internal/brain/rules"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// countingBrain wraps the rules brain and counts generations, so cache
// behaviour is observable.
type countingBrain struct {
	inner *rules.Brain
	calls int
}

func (c *countingBrain) Generate(ctx context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error) {
	c.calls++
	return c.inner.Generate(ctx, snap)
}

func TestBriefingCacheTTL(t *testing.T) {
	cb := &countingBrain{inner: rules.New()}
	a := New(cb, source.Demo{}, discardLogger())

	now := time.Unix(1_760_000_000, 0)
	a.Now = func() time.Time { return now }

	ctx := context.Background()
	req := connect.NewRequest(&kuberov1.GetBriefingRequest{Window: "24h"})

	first, err := a.GetBriefing(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if cb.calls != 1 {
		t.Fatalf("calls = %d, want 1", cb.calls)
	}

	// Within TTL: served from cache, same briefing id.
	now = now.Add(9 * time.Minute)
	second, err := a.GetBriefing(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if cb.calls != 1 {
		t.Errorf("calls = %d after cached read, want 1", cb.calls)
	}
	if first.Msg.Briefing.Id != second.Msg.Briefing.Id {
		t.Error("cached read returned a different briefing")
	}

	// Different window is a different cache key.
	if _, err := a.GetBriefing(ctx, connect.NewRequest(&kuberov1.GetBriefingRequest{Window: "7d"})); err != nil {
		t.Fatal(err)
	}
	if cb.calls != 2 {
		t.Errorf("calls = %d after different window, want 2", cb.calls)
	}

	// Past TTL: regenerated.
	now = now.Add(2 * time.Minute) // 11m after first generation
	third, err := a.GetBriefing(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if cb.calls != 3 {
		t.Errorf("calls = %d after TTL expiry, want 3", cb.calls)
	}
	if third.Msg.Briefing.Id == first.Msg.Briefing.Id {
		t.Error("expired cache entry was served")
	}
}

func TestListAdviceSharesBriefingCache(t *testing.T) {
	cb := &countingBrain{inner: rules.New()}
	a := New(cb, source.Demo{}, discardLogger())

	ctx := context.Background()
	if _, err := a.GetBriefing(ctx, connect.NewRequest(&kuberov1.GetBriefingRequest{Window: "24h"})); err != nil {
		t.Fatal(err)
	}
	res, err := a.ListAdvice(ctx, connect.NewRequest(&kuberov1.ListAdviceRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if cb.calls != 1 {
		t.Errorf("ListAdvice regenerated instead of using the cache (calls=%d)", cb.calls)
	}
	if len(res.Msg.Actions) == 0 {
		t.Error("expected proposed actions from the demo fixture")
	}
	for _, act := range res.Msg.Actions {
		if act.Status != "proposed" {
			t.Errorf("action %s status = %q, want proposed", act.Id, act.Status)
		}
	}
}

// failingSource always errors, exercising the demo degrade path.
type failingSource struct{}

func (failingSource) Fetch(context.Context, string, string) (*source.Snapshot, error) {
	return nil, fmt.Errorf("control-plane unreachable")
}

func TestSourceFailureDegradesToDemo(t *testing.T) {
	a := New(rules.New(), failingSource{}, discardLogger())
	res, err := a.GetBriefing(context.Background(), connect.NewRequest(&kuberov1.GetBriefingRequest{}))
	if err != nil {
		t.Fatalf("GetBriefing must not fail when the source is down: %v", err)
	}
	if res.Msg.Briefing.Source != "demo" {
		t.Errorf("source = %q, want demo when degraded", res.Msg.Briefing.Source)
	}
}

// TestHandlerSmoke spins up the real Connect handler over h2c-less
// httptest and round-trips both RPCs with the generated client. No
// external network involved.
func TestHandlerSmoke(t *testing.T) {
	a := New(rules.New(), source.Demo{}, discardLogger())
	mux := http.NewServeMux()
	path, handler := kuberov1connect.NewAdvisorServiceHandler(a)
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := kuberov1connect.NewAdvisorServiceClient(http.DefaultClient, srv.URL)

	res, err := client.GetBriefing(context.Background(),
		connect.NewRequest(&kuberov1.GetBriefingRequest{ClusterId: "eks-use1-prod", Window: "24h"}))
	if err != nil {
		t.Fatalf("GetBriefing over HTTP: %v", err)
	}
	b := res.Msg.Briefing
	if b.Id == "" || b.GeneratedAtUnix == 0 {
		t.Errorf("briefing missing id/timestamp: %+v", b)
	}
	if b.Source != "demo" {
		t.Errorf("source = %q, want demo", b.Source)
	}
	if b.Headline == "" || b.Markdown == "" || b.SpokenScript == "" {
		t.Error("briefing narrative fields must be populated")
	}
	if len(b.Actions) == 0 {
		t.Fatal("expected actions from demo fixture")
	}

	advice, err := client.ListAdvice(context.Background(),
		connect.NewRequest(&kuberov1.ListAdviceRequest{ClusterId: "eks-use1-prod"}))
	if err != nil {
		t.Fatalf("ListAdvice over HTTP: %v", err)
	}
	if len(advice.Msg.Actions) != len(b.Actions) {
		t.Errorf("ListAdvice actions = %d, GetBriefing actions = %d — should match",
			len(advice.Msg.Actions), len(b.Actions))
	}
}
