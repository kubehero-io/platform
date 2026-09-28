// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/brain/rules"
	"github.com/kubehero-io/platform/services/advisor/internal/investigate"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// countingInvestigator counts runs of the wrapped investigator.
type countingInvestigator struct {
	inner investigate.Investigator
	calls atomic.Int32
}

func (c *countingInvestigator) Investigate(ctx context.Context, r investigate.Request, emit investigate.Emitter) (*kuberov1.InvestigateResponse, error) {
	c.calls.Add(1)
	return c.inner.Investigate(ctx, r, emit)
}

func newInvestigatingAdvisor(t *testing.T) (*Advisor, *countingInvestigator, kuberov1connect.AdvisorServiceClient) {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	demo := backend.Demo{Now: func() time.Time { return now }}
	inv := &countingInvestigator{inner: &investigate.Rules{Backend: demo, Now: func() time.Time { return now }}}
	a := New(rules.New(), source.NewControlPlane(demo), discardLogger())
	a.Investigator = inv
	a.DemoData = true
	clock := now
	a.Now = func() time.Time { return clock }
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewAdvisorServiceHandler(a))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return a, inv, kuberov1connect.NewAdvisorServiceClient(http.DefaultClient, srv.URL)
}

func TestInvestigateStreamSendsStepsThenResult(t *testing.T) {
	_, _, client := newInvestigatingAdvisor(t)
	stream, err := client.InvestigateStream(context.Background(), connect.NewRequest(&kuberov1.InvestigateStreamRequest{
		Request: &kuberov1.InvestigateRequest{Question: "why did checkout's spend jump last night?"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	var steps, progress int
	var result *kuberov1.InvestigateResponse
	for stream.Receive() {
		switch ev := stream.Msg().Event.(type) {
		case *kuberov1.InvestigateStreamResponse_Step:
			if result != nil {
				t.Error("step after the result")
			}
			steps++
		case *kuberov1.InvestigateStreamResponse_Progress:
			progress++
		case *kuberov1.InvestigateStreamResponse_Result:
			result = ev.Result
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Fatal("no terminal result event")
	}
	if steps == 0 || steps != len(result.GetSteps()) || progress == 0 {
		t.Errorf("streamed %d steps / %d progress; result has %d steps", steps, progress, len(result.GetSteps()))
	}
	if result.GetSource() != "demo" || result.GetId() == "" || result.GetGeneratedAtUnix() == 0 {
		t.Errorf("result not stamped / labelled demo: id=%q source=%q", result.GetId(), result.GetSource())
	}
	if result.GetAnswerMarkdown() == "" || len(result.GetEvidence()) == 0 {
		t.Error("empty answer")
	}
}

func TestInvestigateCachesIdenticalQuestionsForAMinute(t *testing.T) {
	a, inv, client := newInvestigatingAdvisor(t)
	ctx := context.Background()
	ask := func(q string) *kuberov1.InvestigateResponse {
		t.Helper()
		res, err := client.Investigate(ctx, connect.NewRequest(&kuberov1.InvestigateRequest{Question: q}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	first := ask("any OOM kills?")
	second := ask("Any OOM kills?") // same question, different case
	if inv.calls.Load() != 1 || first.GetId() != second.GetId() {
		t.Errorf("identical question re-ran (calls=%d)", inv.calls.Load())
	}
	ask("any errors?")
	if inv.calls.Load() != 2 {
		t.Errorf("different question should run (calls=%d)", inv.calls.Load())
	}

	// Past the TTL the question runs again.
	later := time.Date(2026, 9, 28, 12, 1, 1, 0, time.UTC)
	a.Now = func() time.Time { return later }
	third := ask("any OOM kills?")
	if inv.calls.Load() != 3 || third.GetId() == first.GetId() {
		t.Errorf("expired cache entry was served (calls=%d)", inv.calls.Load())
	}

	// A cached answer also streams, straight to the result.
	stream, err := client.InvestigateStream(ctx, connect.NewRequest(&kuberov1.InvestigateStreamRequest{
		Request: &kuberov1.InvestigateRequest{Question: "any OOM kills?"}}))
	if err != nil {
		t.Fatal(err)
	}
	var got *kuberov1.InvestigateResponse
	for stream.Receive() {
		if r := stream.Msg().GetResult(); r != nil {
			got = r
		}
	}
	if got == nil || got.GetId() != third.GetId() || inv.calls.Load() != 3 {
		t.Errorf("cached stream: got %v, calls=%d", got, inv.calls.Load())
	}
}

func TestInvestigateValidation(t *testing.T) {
	_, _, client := newInvestigatingAdvisor(t)
	ctx := context.Background()
	for _, req := range []*kuberov1.InvestigateRequest{
		{Question: ""},
		{Question: "q", Window: "1y"},
	} {
		if _, err := client.Investigate(ctx, connect.NewRequest(req)); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%+v: err = %v, want invalid_argument", req, err)
		}
	}
	stream, err := client.InvestigateStream(ctx, connect.NewRequest(&kuberov1.InvestigateStreamRequest{}))
	if err == nil {
		for stream.Receive() {
		}
		err = stream.Err()
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("stream without request: err = %v", err)
	}
}

func TestInvestigateWithoutInvestigatorIsUnimplemented(t *testing.T) {
	a := New(rules.New(), source.Demo{}, discardLogger())
	_, err := a.Investigate(context.Background(), connect.NewRequest(&kuberov1.InvestigateRequest{Question: "q"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("err = %v", err)
	}
}
