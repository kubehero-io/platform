// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/backend/backendtest"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
)

var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func demoAt() backend.Demo { return backend.Demo{Now: func() time.Time { return testNow }} }

func TestNormalizeRequest(t *testing.T) {
	long := strings.Repeat("x", MaxQuestionLen+1)
	tests := []struct {
		name    string
		in      *kuberov1.InvestigateRequest
		wantErr string
		want    Request
	}{
		{"defaults window", &kuberov1.InvestigateRequest{Question: "  why?  "}, "", Request{Question: "why?", Window: "24h"}},
		{"keeps valid window", &kuberov1.InvestigateRequest{Question: "q", Window: "7d", ClusterId: "eks-1"}, "", Request{Question: "q", Window: "7d", ClusterID: "eks-1"}},
		{"strips control chars", &kuberov1.InvestigateRequest{Question: "a\x1b[31mb\nc"}, "", Request{Question: "a[31mb c", Window: "24h"}},
		{"empty question", &kuberov1.InvestigateRequest{Question: " \t"}, "question is required", Request{}},
		{"long question", &kuberov1.InvestigateRequest{Question: long}, "exceeds", Request{}},
		{"bad window", &kuberov1.InvestigateRequest{Question: "q", Window: "2w"}, "window must be", Request{}},
		{"bad cluster", &kuberov1.InvestigateRequest{Question: "q", ClusterId: "a b"}, "cluster_id", Request{}},
		{"long context", &kuberov1.InvestigateRequest{Question: "q", Context: strings.Repeat("/", MaxContextLen+1)}, "context exceeds", Request{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeRequest(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func newBox(maxCalls int) *Toolbox {
	return NewToolbox(demoAt(), Request{Question: "q", Window: "24h"}, testNow, maxCalls, nil)
}

func TestToolInputValidation(t *testing.T) {
	tests := []struct {
		tool, input, wantErr string
	}{
		{"get_cost_allocation", `{"window":"1y"}`, "window"},
		{"get_cost_allocation", `{"aggregate":["pods"]}`, "aggregate"},
		{"get_cost_allocation", `{"aggregate":["namespace","namespace"]}`, "repeated"},
		{"get_cost_allocation", `{"filters":{"namespace":""}}`, "empty"},
		{"get_cost_allocation", `{"filters":{"secret":"x"}}`, "filter key"},
		{"get_cost_allocation", `{"window":"24h","bogus":1}`, "unknown field"},
		{"get_cost_allocation", `{} {}`, "trailing data"},
		{"get_cost_timeseries", `{"group_by":"pod"}`, "group_by"},
		{"list_rightsizing", `{"namespace":"Not_A_Namespace"}`, "namespace"},
		{"list_rightsizing", `{"min_savings_usd_month":-5}`, "min_savings"},
		{"query_logs", `{}`, "query is required"},
		{"query_logs", `{"query":"error"}`, "stream selector"},
		{"query_logs", `{"query":"{a=\"b\"}","since":"3d"}`, "since"},
		{"query_logs", `{"query":"{a=\"` + strings.Repeat("x", 1001) + `\"}"}`, "exceeds"},
		{"get_log_volume", `{"query":"{a=\"b\"}","group_by":"bad label"}`, "label name"},
		{"get_top_functions", `{}`, "service is required"},
		{"get_top_functions", `{"service":"api","type":"gpu"}`, "type"},
		{"get_flamegraph_summary", `{"service":"../etc"}`, "service is required"},
		{"list_alerts", `{"state":"exploded"}`, "state"},
		{"get_workload", `{"namespace":"payments"}`, "required"},
		{"no_such_tool", `{}`, "unknown tool"},
	}
	for _, tc := range tests {
		t.Run(tc.tool+" "+tc.wantErr, func(t *testing.T) {
			_, err := newBox(50).Run(context.Background(), tc.tool, json.RawMessage(tc.input))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want mention of %q", err, tc.wantErr)
			}
		})
	}
}

func TestToolDefaultsAndClamping(t *testing.T) {
	tb := newBox(10)
	out, err := tb.Run(context.Background(), "get_cost_allocation", json.RawMessage(`{"limit":999}`))
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Window string           `json:"window"`
		Rows   []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("tool result is not JSON: %v · %s", err, out)
	}
	if res.Window != "24h" {
		t.Errorf("window default = %q, want the request window", res.Window)
	}
	steps := tb.Steps()
	if len(steps) != 1 || steps[0].Tool != "get_cost_allocation" || steps[0].Error {
		t.Fatalf("steps = %+v", steps)
	}
	if !strings.Contains(steps[0].InputJson, `"limit":25`) || !strings.Contains(steps[0].InputJson, `"aggregate":["namespace"]`) {
		t.Errorf("normalised input not recorded: %s", steps[0].InputJson)
	}
	if steps[0].Summary == "" {
		t.Error("step summary empty")
	}
}

func TestToolBudget(t *testing.T) {
	tb := newBox(3)
	ctx := context.Background()
	if _, err := tb.Run(ctx, "list_alerts", nil); err != nil {
		t.Fatal(err)
	}
	// Invalid input still spends budget.
	if _, err := tb.Run(ctx, "query_logs", json.RawMessage(`{}`)); err == nil {
		t.Fatal("want invalid input error")
	}
	if _, err := tb.Run(ctx, "list_anomalies", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tb.Run(ctx, "list_anomalies", nil); !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v, want ErrBudget", err)
	}
	steps := tb.Steps()
	if len(steps) != 3 || !steps[1].Error {
		t.Fatalf("steps = %+v", steps)
	}
}

// noisy returns oversized log lines to exercise truncation.
type noisy struct{ backend.Demo }

func (noisy) QueryLogs(context.Context, *kuberov1.QueryLogsRequest) (*kuberov1.QueryLogsResponse, error) {
	var lines []*kuberov1.LogLine
	for i := 0; i < 200; i++ {
		lines = append(lines, &kuberov1.LogLine{TsUnixNano: testNow.UnixNano(), Body: strings.Repeat("é", 1000), Level: "error"})
	}
	return &kuberov1.QueryLogsResponse{ResultType: "streams", Lines: lines}, nil
}

func TestToolResultsStaySmall(t *testing.T) {
	tb := NewToolbox(noisy{}, Request{Question: "q", Window: "1h"}, testNow, 5, nil)
	out, err := tb.Run(context.Background(), "query_logs", json.RawMessage(`{"query":"{namespace=\"x\"}","limit":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > maxResultBytes {
		t.Fatalf("result %d bytes exceeds the %d cap", len(out), maxResultBytes)
	}
	if !json.Valid([]byte(out)) {
		t.Fatalf("truncated result must stay valid JSON: %.200s", out)
	}
	// 50 lines × 300 runes of 2-byte chars can't fit: the size guard kicks in.
	if !strings.Contains(out, `"truncated":true`) {
		t.Errorf("expected the truncation marker: %.200s", out)
	}
}

func TestSpecsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Specs() {
		if seen[s.Name] {
			t.Errorf("duplicate tool %s", s.Name)
		}
		seen[s.Name] = true
		if len(s.Description) < 40 {
			t.Errorf("%s: description too thin", s.Name)
		}
		if s.Schema["type"] != "object" || s.Schema["additionalProperties"] != false {
			t.Errorf("%s: schema must be a closed object", s.Name)
		}
		if _, err := json.Marshal(s.Schema); err != nil {
			t.Errorf("%s: schema not JSON: %v", s.Name, err)
		}
	}
	for _, want := range []string{
		"get_cost_allocation", "get_cost_timeseries", "list_anomalies", "list_rightsizing", "query_logs",
		"get_log_patterns", "get_log_volume", "get_top_functions", "get_flamegraph_summary", "get_service_map",
		"list_network_costs", "list_alerts", "list_capacity_demands", "get_workload",
	} {
		if !seen[want] {
			t.Errorf("missing tool %s", want)
		}
	}
}

func TestDetectIntents(t *testing.T) {
	tests := []struct {
		q    string
		want []intent
	}{
		{"why did checkout's spend jump last night?", []intent{intentCost}},
		{"errors in payments", []intent{intentAlerts, intentErrors}},
		{"is anything slow?", []intent{intentPerf}},
		{"any OOM kills?", []intent{intentMemory}},
		{"what's our cross zone traffic", []intent{intentNetwork}},
		{"how are we doing", []intent{intentAlerts, intentCost, intentErrors}},
	}
	for _, tc := range tests {
		got := detectIntents(tc.q)
		if len(got) != len(tc.want) {
			t.Errorf("%q: got %v want %v", tc.q, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%q: got %v want %v", tc.q, got, tc.want)
			}
		}
	}
}

// rulesOverWire runs the rules investigator against the demo fixture
// served by a real Connect fake control plane (with auth).
func rulesOverWire(t *testing.T) *Rules {
	t.Helper()
	srv := backendtest.New(t, demoAt(), "tok")
	return &Rules{Backend: backend.NewConnect(srv.URL, "tok"), Now: func() time.Time { return testNow }}
}

func TestRulesInvestigations(t *testing.T) {
	r := rulesOverWire(t)
	tests := []struct {
		name         string
		req          Request
		wantSubject  string
		wantTools    []string
		wantEvidence []string // kinds
		wantInAnswer []string
		wantKinds    []string // action kinds
	}{
		{
			name:         "cost jump correlates errors and changes",
			req:          Request{Question: "Why did checkout's spend jump last night?", Window: "24h"},
			wantSubject:  "payments/checkout-api",
			wantTools:    []string{"get_cost_allocation", "get_workload", "get_cost_timeseries", "list_alerts", "get_log_volume", "get_log_patterns"},
			wantEvidence: []string{"cost", "logs", "alert", "event"},
			wantInAnswer: []string{"rose", "upstream timeout", "PaymentsErrorRate", "pool size 50→20"},
			wantKinds:    []string{brain.KindInvestigate},
		},
		{
			name:         "slowness reads profiles",
			req:          Request{Question: "is anything slow in payments?", Window: "24h"},
			wantSubject:  "payments",
			wantTools:    []string{"get_top_functions", "get_flamegraph_summary"},
			wantEvidence: []string{"profile"},
			wantInAnswer: []string{"crypto/tls.(*Conn).Handshake", "regression"},
		},
		{
			name:         "network spend",
			req:          Request{Question: "what is our network egress costing us?", Window: "7d"},
			wantSubject:  "the fleet",
			wantTools:    []string{"list_network_costs", "get_service_map"},
			wantEvidence: []string{"network"},
			wantInAnswer: []string{"edge/api-gateway", "retransmits"},
		},
		{
			name:         "oom question",
			req:          Request{Question: "any OOM kills in ml-inference?", Window: "24h"},
			wantSubject:  "ml-inference",
			wantTools:    []string{"list_rightsizing"},
			wantEvidence: []string{"rightsizing"},
			wantInAnswer: []string{"OOM-killed 4 times"},
			wantKinds:    []string{brain.KindInvestigate},
		},
		{
			name:         "over-provisioning proposes a recommend-mode policy",
			req:          Request{Question: "where is payments over-provisioned? rightsize it", Window: "24h"},
			wantSubject:  "payments",
			wantTools:    []string{"list_rightsizing"},
			wantEvidence: []string{"rightsizing"},
			wantInAnswer: []string{"over-provisioned"},
			wantKinds:    []string{brain.KindRightsize},
		},
		{
			name:         "dashboard context names the subject",
			req:          Request{Question: "why is this so expensive?", Window: "24h", Context: "/workloads/gke-euw1-prod/data/batch-etl"},
			wantSubject:  "data/batch-etl",
			wantTools:    []string{"get_workload", "get_cost_timeseries"},
			wantInAnswer: []string{"batch-etl"},
		},
		{
			name:         "open question gets a health check",
			req:          Request{Question: "how are we doing?", Window: "24h"},
			wantSubject:  "the fleet",
			wantTools:    []string{"list_alerts", "get_cost_timeseries", "get_log_volume"},
			wantEvidence: []string{"alert", "logs"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := r.Investigate(context.Background(), tc.req, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Source != "rules" {
				t.Errorf("source = %q", res.Source)
			}
			if !strings.Contains(res.AnswerMarkdown, "What I found about "+tc.wantSubject) {
				t.Errorf("subject: answer starts %q", firstLine(res.AnswerMarkdown))
			}
			tools := map[string]bool{}
			for _, s := range res.Steps {
				tools[s.Tool] = true
				if s.Error {
					t.Errorf("step %s failed: %s", s.Tool, s.Summary)
				}
				if s.Summary == "" || s.InputJson == "" {
					t.Errorf("step %s missing summary/input", s.Tool)
				}
			}
			if len(res.Steps) > DefaultMaxCalls {
				t.Errorf("%d steps exceed the budget", len(res.Steps))
			}
			for _, want := range tc.wantTools {
				if !tools[want] {
					t.Errorf("tool %s not called (called: %v)", want, keysOf(tools))
				}
			}
			kinds := map[string]bool{}
			for _, e := range res.Evidence {
				kinds[e.Kind] = true
				if e.LinkPath != "" && !strings.HasPrefix(e.LinkPath, "/") {
					t.Errorf("evidence link %q is not dashboard-relative", e.LinkPath)
				}
			}
			for _, want := range tc.wantEvidence {
				if !kinds[want] {
					t.Errorf("no %s evidence (got %v)", want, keysOf(kinds))
				}
			}
			for _, want := range tc.wantInAnswer {
				if !strings.Contains(res.AnswerMarkdown, want) {
					t.Errorf("answer missing %q:\n%s", want, res.AnswerMarkdown)
				}
			}
			actionKinds := map[string]bool{}
			for _, a := range res.Actions {
				actionKinds[a.Kind] = true
				if a.Status != brain.StatusProposed {
					t.Errorf("action %s status %q", a.Id, a.Status)
				}
				if a.Kind == brain.KindRightsize && !strings.Contains(a.CrdYaml, "mode: recommend") {
					t.Errorf("rightsize proposal must be recommend-mode:\n%s", a.CrdYaml)
				}
			}
			for _, want := range tc.wantKinds {
				if !actionKinds[want] {
					t.Errorf("no %s action (got %v)", want, keysOf(actionKinds))
				}
			}
			if res.SpokenSummary == "" || strings.ContainsAny(res.SpokenSummary, "*#`") {
				t.Errorf("spoken summary must be plain prose: %q", res.SpokenSummary)
			}

			again, _ := r.Investigate(context.Background(), tc.req, nil)
			if again.AnswerMarkdown != res.AnswerMarkdown || again.SpokenSummary != res.SpokenSummary {
				t.Error("rules investigation must be deterministic")
			}
		})
	}
}

func TestRulesStreamsSteps(t *testing.T) {
	var mu sync.Mutex
	var steps, progress int
	emit := func(ev *kuberov1.InvestigateStreamResponse) {
		mu.Lock()
		defer mu.Unlock()
		switch ev.Event.(type) {
		case *kuberov1.InvestigateStreamResponse_Step:
			steps++
		case *kuberov1.InvestigateStreamResponse_Progress:
			progress++
		}
	}
	res, err := (&Rules{Backend: demoAt(), Now: func() time.Time { return testNow }}).
		Investigate(context.Background(), Request{Question: "errors in payments", Window: "1h"}, emit)
	if err != nil {
		t.Fatal(err)
	}
	if steps != len(res.Steps) || steps == 0 {
		t.Errorf("streamed %d steps, response has %d", steps, len(res.Steps))
	}
	if progress == 0 {
		t.Error("no progress events")
	}
}

// broken fails every logs RPC.
type broken struct{ backend.Demo }

func (broken) GetLogVolume(context.Context, *kuberov1.GetLogVolumeRequest) (*kuberov1.GetLogVolumeResponse, error) {
	return nil, errors.New("clickhouse unavailable")
}

func TestRulesSaysWhatItCouldNotCheck(t *testing.T) {
	res, err := (&Rules{Backend: broken{demoAt()}, Now: func() time.Time { return testNow }}).
		Investigate(context.Background(), Request{Question: "any errors?", Window: "24h"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.AnswerMarkdown, "Couldn't check") || !strings.Contains(res.AnswerMarkdown, "clickhouse unavailable") {
		t.Errorf("failed tool not surfaced:\n%s", res.AnswerMarkdown)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
