// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/backend/backendtest"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/investigate"
)

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

const validPolicy = `apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata:
  name: advisor-rightsize-checkout-api
  namespace: kubehero-system
spec:
  scope:
    namespaceSelector:
      matchLabels: { kubernetes.io/metadata.name: payments }
  mode: recommend
`

func finalAnswer() string {
	return mustJSON(map[string]any{
		"answer_markdown": "checkout-api spend rose 44% because retries from upstream timeouts scaled it out.",
		"spoken_summary":  "Checkout's spend rose because upstream timeouts caused retries and a scale-out.",
		"evidence": []map[string]any{
			{"kind": "cost", "title": "checkout-api spend +44%", "detail": "last 6 hours", "link_path": "/allocation?aggregate=workload", "query": ""},
			{"kind": "logs", "title": "upstream timeout pattern", "detail": "18,240 lines", "link_path": "https://evil.example/phish", "query": `{namespace="payments"}`},
			{"kind": "metrics", "title": "made-up kind", "detail": "", "link_path": "//evil.example", "query": ""},
		},
		"actions": []map[string]any{
			{"id": "act-1", "title": "Delete the deployment", "impact_monthly_usd": 100, "risk": "low",
				"kind": "kubectl.delete", "target": "payments/checkout-api", "rationale": "yolo", "crd_yaml": "kind: Deployment"},
			{"id": "act-2", "title": "Rightsize checkout-api", "impact_monthly_usd": 6200, "risk": "low",
				"kind": brain.KindRightsize, "target": "eks-use1-prod/payments/checkout-api", "rationale": "cpu 16 vs 3.1 used", "crd_yaml": validPolicy},
			{"id": "act-3", "title": "Sneaky", "impact_monthly_usd": -50, "risk": "extreme",
				"kind": brain.KindRightsize, "target": "x", "rationale": "bad manifest", "crd_yaml": "apiVersion: v1\nkind: ConfigMap\n"},
		},
	})
}

func newTestInvestigator(t *testing.T, f *fakeAnthropic, logBuf *bytes.Buffer) (*Investigator, *backendtest.Server) {
	t.Helper()
	demo := backend.Demo{Now: func() time.Time { return testNow }}
	cp := backendtest.New(t, demo, "cp-token")
	be := backend.NewConnect(cp.URL, "cp-token")
	log := slog.New(slog.NewJSONHandler(logBuf, nil))
	rules := &investigate.Rules{Backend: be, Now: func() time.Time { return testNow }}
	inv := NewInvestigator(be, rules, log, f.config())
	inv.Now = func() time.Time { return testNow }
	return inv, cp
}

func TestInvestigatorRunsToolLoopAndGuardsOutput(t *testing.T) {
	f := newFakeAnthropic(t,
		fakeTurn{toolCalls: []fakeToolCall{
			{name: "get_cost_timeseries", input: `{"group_by":"workload","filters":{"namespace":"payments"}}`},
			{name: "query_logs", input: `{"query":"error","limit":5}`}, // invalid: no stream selector
		}},
		fakeTurn{toolCalls: []fakeToolCall{
			{name: "get_log_patterns", input: `{"query":"{namespace=\"payments\"}","limit":3}`},
		}},
		fakeTurn{text: finalAnswer()},
	)
	var logs bytes.Buffer
	inv, cp := newTestInvestigator(t, f, &logs)

	var events []*kuberov1.InvestigateStreamResponse
	emit := func(ev *kuberov1.InvestigateStreamResponse) { events = append(events, ev) }
	res, err := inv.Investigate(context.Background(),
		investigate.Request{Question: "why did checkout's spend jump?", Window: "24h"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "llm" {
		t.Fatalf("source = %q (logs: %s)", res.Source, logs.String())
	}

	// Executing the model's tool calls against the control plane.
	if cp.Calls("GetCostTimeseries") != 1 || cp.Calls("GetLogPatterns") != 1 {
		t.Errorf("tool calls not executed: timeseries=%d patterns=%d", cp.Calls("GetCostTimeseries"), cp.Calls("GetLogPatterns"))
	}
	if len(res.Steps) != 3 {
		t.Fatalf("steps = %d, want 3: %+v", len(res.Steps), res.Steps)
	}
	var sawInvalid bool
	for _, s := range res.Steps {
		if s.Tool == "query_logs" && s.Error {
			sawInvalid = true
		}
	}
	if !sawInvalid {
		t.Error("invalid tool input should be recorded as an error step")
	}

	// Returning tool errors to the model as is_error tool_results.
	results := toolResults(f.request(1))
	if len(results) != 2 {
		t.Fatalf("second request carries %d tool results, want 2", len(results))
	}
	var errResult, okResult bool
	for _, r := range results {
		if r["is_error"] == true {
			errResult = true
		} else {
			okResult = true
		}
	}
	if !errResult || !okResult {
		t.Errorf("want one error and one ok tool_result, got %+v", results)
	}

	// Guarding the model's output.
	if len(res.Actions) != 3 {
		t.Fatalf("actions = %d", len(res.Actions))
	}
	for _, a := range res.Actions {
		if a.Status != brain.StatusProposed {
			t.Errorf("%s status %q", a.Id, a.Status)
		}
		switch a.Id {
		case "act-1", "act-3":
			if a.Kind != brain.KindInvestigate || a.CrdYaml != "" {
				t.Errorf("%s should be downgraded to investigate-only: %+v", a.Id, a)
			}
		case "act-2":
			if a.Kind != brain.KindRightsize || a.CrdYaml == "" {
				t.Errorf("valid proposal was downgraded: %+v", a)
			}
		}
		if a.ImpactMonthlyUsd < 0 {
			t.Errorf("%s negative impact survived", a.Id)
		}
	}
	for _, e := range res.Evidence {
		if strings.Contains(e.LinkPath, "evil") {
			t.Errorf("external link survived: %q", e.LinkPath)
		}
	}
	if res.Evidence[2].Kind != "event" {
		t.Errorf("unknown evidence kind should map to event, got %q", res.Evidence[2].Kind)
	}

	// Streaming steps and progress.
	var steps, progress int
	for _, ev := range events {
		switch ev.Event.(type) {
		case *kuberov1.InvestigateStreamResponse_Step:
			steps++
		case *kuberov1.InvestigateStreamResponse_Progress:
			progress++
		}
	}
	if steps != 3 || progress < 3 {
		t.Errorf("streamed %d steps / %d progress events", steps, progress)
	}

	// Sending the Opus 5 request shape.
	req := f.request(0)
	if req["model"] != DefaultModel {
		t.Errorf("model = %v", req["model"])
	}
	if req["fallbacks"] != "default" {
		t.Errorf("fallbacks = %v, want \"default\"", req["fallbacks"])
	}
	if !hasBeta(f.betas[0], "server-side-fallback-2026-07-01") {
		t.Errorf("anthropic-beta = %q", f.betas[0])
	}
	if th, _ := req["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Errorf("thinking = %v", req["thinking"])
	}
	for _, banned := range []string{"temperature", "top_p", "top_k"} {
		if _, ok := req[banned]; ok {
			t.Errorf("%s must not be sent", banned)
		}
	}
	if req["stream"] != true {
		t.Error("requests must stream")
	}
	oc, _ := req["output_config"].(map[string]any)
	if format, _ := oc["format"].(map[string]any); format["type"] != "json_schema" {
		t.Errorf("output_config = %v", req["output_config"])
	}
	if tools, _ := req["tools"].([]any); len(tools) != len(investigate.Specs()) {
		t.Errorf("tools = %d, want %d", len(tools), len(investigate.Specs()))
	}
	if sys, _ := req["system"].([]any); len(sys) != 1 || !strings.Contains(mustJSON(sys), "cache_control") {
		t.Errorf("system prompt should be cached: %v", req["system"])
	}
	msgs, _ := req["messages"].([]any)
	if last, _ := msgs[len(msgs)-1].(map[string]any); last["role"] != "user" {
		t.Error("no assistant prefill: the last message must be the user's")
	}
}

func TestInvestigatorRefusalFallsBackToRules(t *testing.T) {
	f := newFakeAnthropic(t, fakeTurn{refusal: "cyber"})
	var logs bytes.Buffer
	inv, _ := newTestInvestigator(t, f, &logs)
	res, err := inv.Investigate(context.Background(), investigate.Request{Question: "any errors in payments?", Window: "24h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "rules" {
		t.Errorf("source = %q, want rules fallback", res.Source)
	}
	if !strings.Contains(logs.String(), `"refusal_category":"cyber"`) {
		t.Errorf("refusal category not logged: %s", logs.String())
	}
	if len(res.Steps) == 0 {
		t.Error("rules fallback should still investigate")
	}
}

func TestInvestigatorAPIErrorFallsBackToRules(t *testing.T) {
	f := newFakeAnthropic(t, fakeTurn{status: 400})
	var logs bytes.Buffer
	inv, _ := newTestInvestigator(t, f, &logs)
	res, err := inv.Investigate(context.Background(), investigate.Request{Question: "how are we doing?", Window: "24h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "rules" {
		t.Errorf("source = %q, want rules fallback", res.Source)
	}
}

func TestInvestigatorGarbageFallsBackToRules(t *testing.T) {
	f := newFakeAnthropic(t, fakeTurn{text: "I think it's fine, honestly."})
	var logs bytes.Buffer
	inv, _ := newTestInvestigator(t, f, &logs)
	res, err := inv.Investigate(context.Background(), investigate.Request{Question: "how are we doing?", Window: "24h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "rules" {
		t.Errorf("source = %q, want rules fallback", res.Source)
	}
}

func TestInvestigatorRespectsToolBudget(t *testing.T) {
	// A model that never stops calling tools runs out of budget and turns;
	// the investigation then falls back instead of looping forever.
	var turns []fakeTurn
	for i := 0; i < 30; i++ {
		turns = append(turns, fakeTurn{toolCalls: []fakeToolCall{{name: "list_alerts", input: `{}`}}})
	}
	f := newFakeAnthropic(t, turns...)
	var logs bytes.Buffer
	inv, cp := newTestInvestigator(t, f, &logs)
	inv.MaxCalls = 4
	res, err := inv.Investigate(context.Background(), investigate.Request{Question: "any alerts?", Window: "24h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "rules" {
		t.Errorf("source = %q, want rules after the loop is cut off", res.Source)
	}
	if f.count() > inv.MaxCalls+3 {
		t.Errorf("%d model turns for a budget of %d", f.count(), inv.MaxCalls)
	}
	// 4 budgeted LLM tool calls + the rules fallback's own call.
	if got := cp.Calls("ListAlerts"); got > inv.MaxCalls+1 {
		t.Errorf("ListAlerts hit %d times; budget not enforced", got)
	}
}

func TestParseInvestigation(t *testing.T) {
	if _, err := parseInvestigation(`{"answer_markdown":"","spoken_summary":"x","evidence":[],"actions":[]}`); err == nil {
		t.Error("empty answer must be rejected")
	}
	res, err := parseInvestigation("```json\n" + `{"answer_markdown":"## Answer\nIt is fine.","evidence":[],"actions":[]}` + "\n```")
	if err != nil {
		t.Fatal(err)
	}
	if res.SpokenSummary == "" {
		t.Error("spoken summary should fall back to the first sentence")
	}
	for _, tc := range []struct{ in, want string }{
		{"/logs?query=x", "/logs?query=x"},
		{"https://example.com", ""},
		{"//example.com/x", ""},
		{"javascript:alert(1)", ""},
		{"/ok path", ""},
	} {
		if got := safeLinkPath(tc.in); got != tc.want {
			t.Errorf("safeLinkPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
