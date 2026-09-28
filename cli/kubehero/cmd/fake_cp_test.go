// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

// fakeCP is a control plane + advisor served through the generated
// Connect handlers, so CLI tests cover the real client, codec and auth.
type fakeCP struct {
	kuberov1connect.UnimplementedControlPlaneServiceHandler
	kuberov1connect.UnimplementedCostServiceHandler
	kuberov1connect.UnimplementedLogsServiceHandler
	kuberov1connect.UnimplementedProfilesServiceHandler
	kuberov1connect.UnimplementedNetworkServiceHandler
	kuberov1connect.UnimplementedAlertsServiceHandler
	kuberov1connect.UnimplementedAdvisorServiceHandler

	token string
	srv   *httptest.Server

	mu    sync.Mutex
	reqs  map[string]any
	auths map[string]string
	focus string // last export query string
}

func newFakeCP(t *testing.T, token string) *fakeCP {
	t.Helper()
	f := &fakeCP{token: token, reqs: map[string]any{}, auths: map[string]string{}}
	auth := connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if f.token != "" && req.Header().Get("Authorization") != "Bearer "+f.token {
				return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("bad token"))
			}
			return next(ctx, req)
		}
	}))
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewControlPlaneServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewCostServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewLogsServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewProfilesServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewNetworkServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewAlertsServiceHandler(f, auth))
	mux.Handle(kuberov1connect.NewAdvisorServiceHandler(f, auth))
	mux.HandleFunc("/api/v1/export/focus", func(w http.ResponseWriter, r *http.Request) {
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.focus = r.URL.RawQuery
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/csv")
		fmt.Fprint(w, "BilledCost,EffectiveCost,ResourceId\n12.50,12.50,eks-1/payments/api\n")
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func record[T any](f *fakeCP, method string, req *connect.Request[T]) *T {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs[method] = req.Msg
	f.auths[method] = req.Header().Get("Authorization")
	return req.Msg
}

func (f *fakeCP) got(method string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[method]
}

var fakeNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// ─── control plane ───────────────────────────────────────────────────────

func (f *fakeCP) WhoAmI(_ context.Context, r *connect.Request[kuberov1.WhoAmIRequest]) (*connect.Response[kuberov1.WhoAmIResponse], error) {
	record(f, "WhoAmI", r)
	role, sub := "anonymous", "anonymous"
	if r.Header().Get("Authorization") != "" {
		role, sub = "member", "key:3f9a1c2e"
	}
	return connect.NewResponse(&kuberov1.WhoAmIResponse{Subject: sub, Role: role, AuthRequired: f.token != ""}), nil
}

func (f *fakeCP) ListClusters(_ context.Context, r *connect.Request[kuberov1.ListClustersRequest]) (*connect.Response[kuberov1.ListClustersResponse], error) {
	record(f, "ListClusters", r)
	return connect.NewResponse(&kuberov1.ListClustersResponse{Clusters: []*kuberov1.Cluster{
		{Id: "eks-use1-prod", Name: "eks-use1-prod", Cloud: "aws", Region: "us-east-1", Nodes: 42}}}), nil
}

func (f *fakeCP) ListAnomalies(_ context.Context, r *connect.Request[kuberov1.ListAnomaliesRequest]) (*connect.Response[kuberov1.ListAnomaliesResponse], error) {
	record(f, "ListAnomalies", r)
	return connect.NewResponse(&kuberov1.ListAnomaliesResponse{Anomalies: []*kuberov1.Anomaly{
		{Id: "anom-1", Title: "payments spend +28%", ImpactUsdMonth: 8600, Severity: "warn"}}, Total: 1}), nil
}

func (f *fakeCP) AppendAuditEntry(_ context.Context, r *connect.Request[kuberov1.AppendAuditEntryRequest]) (*connect.Response[kuberov1.AppendAuditEntryResponse], error) {
	record(f, "AppendAuditEntry", r)
	return connect.NewResponse(&kuberov1.AppendAuditEntryResponse{Id: 42}), nil
}

// ─── cost ────────────────────────────────────────────────────────────────

func (f *fakeCP) GetAllocation(_ context.Context, r *connect.Request[kuberov1.GetAllocationRequest]) (*connect.Response[kuberov1.GetAllocationResponse], error) {
	record(f, "GetAllocation", r)
	return connect.NewResponse(&kuberov1.GetAllocationResponse{
		Allocations: []*kuberov1.Allocation{
			{Name: "payments", TotalCost: 7234.5, CpuCost: 5000, RamCost: 2234.5, CpuEfficiency: 0.26, RamEfficiency: 0.52, RecoverableCost: 1400},
			{Name: "data", TotalCost: 2916, CpuEfficiency: 0.55, RamEfficiency: 0.17},
		},
		Totals: &kuberov1.Allocation{TotalCost: 10150.5, TotalEfficiency: 0.4, RecoverableCost: 1400},
		Source: "live",
	}), nil
}

func (f *fakeCP) GetCostTimeseries(_ context.Context, r *connect.Request[kuberov1.GetCostTimeseriesRequest]) (*connect.Response[kuberov1.GetCostTimeseriesResponse], error) {
	record(f, "GetCostTimeseries", r)
	pts := []*kuberov1.Point{}
	for i := 0; i < 10; i++ {
		pts = append(pts, &kuberov1.Point{TsUnixMs: fakeNow.Add(time.Duration(i-10) * 24 * time.Hour).UnixMilli(), Value: float64(100 + i*10)})
	}
	return connect.NewResponse(&kuberov1.GetCostTimeseriesResponse{
		Series:   []*kuberov1.Series{{Labels: map[string]string{"namespace": "payments"}, Points: pts}},
		TotalUsd: 1450, ForecastMonthUsd: 4800, Source: "demo",
	}), nil
}

func (f *fakeCP) ListRightsizing(_ context.Context, r *connect.Request[kuberov1.ListRightsizingRequest]) (*connect.Response[kuberov1.ListRightsizingResponse], error) {
	record(f, "ListRightsizing", r)
	return connect.NewResponse(&kuberov1.ListRightsizingResponse{Recommendations: []*kuberov1.RightsizingRecommendation{
		{Namespace: "payments", Workload: "checkout-api", Container: "app", CpuRequestCores: 16, CpuRecommendedCores: 3.6,
			MemRequestBytes: 32 << 30, MemRecommendedBytes: 18 << 30, SavingsUsdMonth: 6200, Confidence: "high", Direction: "downsize",
			Reason: "p95 CPU 3.1 of 16 cores"},
		{Namespace: "ml", Workload: "embedder", Container: "embedder", CpuRequestCores: 6, CpuRecommendedCores: 4.5,
			MemRequestBytes: 24 << 30, MemRecommendedBytes: 28 << 30, SavingsUsdMonth: -310, Confidence: "high", Direction: "upsize", OomKills: 4},
	}, TotalSavingsUsdMonth: 5890, Source: "live"}), nil
}

func (f *fakeCP) GetEfficiency(_ context.Context, r *connect.Request[kuberov1.GetEfficiencyRequest]) (*connect.Response[kuberov1.GetEfficiencyResponse], error) {
	record(f, "GetEfficiency", r)
	return connect.NewResponse(&kuberov1.GetEfficiencyResponse{Score: 41, CpuEfficiency: 0.31, RamEfficiency: 0.43,
		Namespaces: []*kuberov1.EfficiencyBreakdown{{Name: "payments", Score: 45, CpuEfficiency: 0.36}}}), nil
}

// ─── logs ────────────────────────────────────────────────────────────────

func (f *fakeCP) QueryLogs(_ context.Context, r *connect.Request[kuberov1.QueryLogsRequest]) (*connect.Response[kuberov1.QueryLogsResponse], error) {
	m := record(f, "QueryLogs", r)
	if len(m.GetQuery()) > 0 && m.GetQuery()[0] == 's' { // sum(...) → metric query
		return connect.NewResponse(&kuberov1.QueryLogsResponse{ResultType: "matrix", Series: []*kuberov1.Series{
			{Labels: map[string]string{"namespace": "payments"}, Points: []*kuberov1.Point{{Value: 3}, {Value: 42}}}}}), nil
	}
	return connect.NewResponse(&kuberov1.QueryLogsResponse{ResultType: "streams", Lines: []*kuberov1.LogLine{
		{TsUnixNano: fakeNow.UnixNano(), Level: "error", Body: "upstream timeout calling payments-db after 3000ms",
			Labels: map[string]string{"namespace": "payments", "workload": "checkout-api"}},
	}}), nil
}

func (f *fakeCP) GetLogVolume(_ context.Context, r *connect.Request[kuberov1.GetLogVolumeRequest]) (*connect.Response[kuberov1.GetLogVolumeResponse], error) {
	record(f, "GetLogVolume", r)
	return connect.NewResponse(&kuberov1.GetLogVolumeResponse{Series: []*kuberov1.Series{
		{Labels: map[string]string{"level": "error"}, Points: []*kuberov1.Point{{Value: 10}, {Value: 900}}}},
		TotalLines: 910, TotalBytes: 282100, EstCostUsdMonth: 1.25}), nil
}

func (f *fakeCP) GetLogPatterns(_ context.Context, r *connect.Request[kuberov1.GetLogPatternsRequest]) (*connect.Response[kuberov1.GetLogPatternsResponse], error) {
	record(f, "GetLogPatterns", r)
	return connect.NewResponse(&kuberov1.GetLogPatternsResponse{Patterns: []*kuberov1.LogPattern{
		{Pattern: "upstream timeout calling <_> after <_>ms", Count: 18240, Level: "error", SharePct: 61, Sample: "upstream timeout calling db after 3000ms"},
	}, LinesAnalyzed: 30100}), nil
}

func (f *fakeCP) TailLogs(_ context.Context, r *connect.Request[kuberov1.TailLogsRequest], s *connect.ServerStream[kuberov1.TailLogsResponse]) error {
	record(f, "TailLogs", r)
	for i := 0; i < 2; i++ {
		if err := s.Send(&kuberov1.TailLogsResponse{Lines: []*kuberov1.LogLine{
			{TsUnixNano: fakeNow.Add(time.Duration(i) * time.Second).UnixNano(), Level: "warn", Body: fmt.Sprintf("tail line %d", i)}},
			Dropped: int64(i)}); err != nil {
			return err
		}
	}
	return nil
}

// ─── profiles ────────────────────────────────────────────────────────────

func (f *fakeCP) GetTopFunctions(_ context.Context, r *connect.Request[kuberov1.GetTopFunctionsRequest]) (*connect.Response[kuberov1.GetTopFunctionsResponse], error) {
	record(f, "GetTopFunctions", r)
	return connect.NewResponse(&kuberov1.GetTopFunctionsResponse{Functions: []*kuberov1.TopFunction{
		{Name: "crypto/tls.(*Conn).Handshake", SelfPct: 35.1, TotalPct: 35.1, SelfCostUsdMonth: 4770},
	}, Total: 6000, Unit: "samples"}), nil
}

// flameFixture: total → handler → {tls (regressed), json}.
func flameFixture() *kuberov1.GetFlamegraphResponse {
	return &kuberov1.GetFlamegraphResponse{
		Nodes: []*kuberov1.FlameNode{
			{Name: "total", Parent: -1, Total: 1000, BaselineTotal: 1000},
			{Name: "main.handleCharge", Parent: 0, Depth: 1, Total: 900, Self: 20, BaselineTotal: 800},
			{Name: "crypto/tls.(*Conn).Handshake", Parent: 1, Depth: 2, Total: 600, Self: 600, BaselineTotal: 200},
			{Name: "encoding/json.Marshal", Parent: 1, Depth: 2, Total: 280, Self: 280, BaselineTotal: 580},
			{Name: "tiny", Parent: 0, Depth: 1, Total: 5, Self: 5},
		},
		Total: 1000, BaselineTotal: 1000, Samples: 1000, Type: "cpu", Unit: "samples", CostUsdMonth: 13650,
	}
}

func (f *fakeCP) GetFlamegraph(_ context.Context, r *connect.Request[kuberov1.GetFlamegraphRequest]) (*connect.Response[kuberov1.GetFlamegraphResponse], error) {
	record(f, "GetFlamegraph", r)
	return connect.NewResponse(flameFixture()), nil
}

func (f *fakeCP) ListProfileTargets(_ context.Context, r *connect.Request[kuberov1.ListProfileTargetsRequest]) (*connect.Response[kuberov1.ListProfileTargetsResponse], error) {
	record(f, "ListProfileTargets", r)
	return connect.NewResponse(&kuberov1.ListProfileTargetsResponse{Targets: []*kuberov1.ProfileTarget{
		{Service: "checkout-api", Namespace: "payments", Types: []string{"cpu"}, Origin: "ebpf", CpuCoresAvg: 41.6, CostUsdMonth: 19500}}}), nil
}

// ─── network ─────────────────────────────────────────────────────────────

func (f *fakeCP) GetServiceMap(_ context.Context, r *connect.Request[kuberov1.GetServiceMapRequest]) (*connect.Response[kuberov1.GetServiceMapResponse], error) {
	record(f, "GetServiceMap", r)
	return connect.NewResponse(&kuberov1.GetServiceMapResponse{
		Nodes: []*kuberov1.ServiceMapNode{{Id: "workload:edge/api-gateway"}, {Id: "external:internet"}},
		Edges: []*kuberov1.ServiceMapEdge{
			{Source: "workload:payments/checkout-api", Target: "service:payments/db", Port: 5432, Bytes: 6.4e10, CostUsdMonth: 640, CrossZone: true, Retransmits: 18400},
			{Source: "workload:edge/api-gateway", Target: "external:internet", Port: 443, Bytes: 4.1e12, CostUsdMonth: 2100, Egress: true},
		},
		TotalCostUsdMonth: 2740, EgressGb: 4100, CrossZoneGb: 64,
	}), nil
}

func (f *fakeCP) ListNetworkCosts(_ context.Context, r *connect.Request[kuberov1.ListNetworkCostsRequest]) (*connect.Response[kuberov1.ListNetworkCostsResponse], error) {
	record(f, "ListNetworkCosts", r)
	return connect.NewResponse(&kuberov1.ListNetworkCostsResponse{Costs: []*kuberov1.NetworkCost{
		{Namespace: "edge", Workload: "api-gateway", EgressGb: 4100, EgressUsdMonth: 2100, TotalUsdMonth: 2100, TopDestination: "internet"}},
		TotalUsdMonth: 2100}), nil
}

// ─── alerts ──────────────────────────────────────────────────────────────

func (f *fakeCP) ListAlerts(_ context.Context, r *connect.Request[kuberov1.ListAlertsRequest]) (*connect.Response[kuberov1.ListAlertsResponse], error) {
	record(f, "ListAlerts", r)
	return connect.NewResponse(&kuberov1.ListAlertsResponse{Alerts: []*kuberov1.Alert{
		{RuleName: "PaymentsErrorRate", State: "firing", Severity: "critical", Value: 6.3, Summary: "payments error rate 6.3%",
			Labels: map[string]string{"namespace": "payments"}, FiredAt: "2026-09-28T02:05:00Z"}}, Firing: 1}), nil
}

func (f *fakeCP) ListAlertRules(_ context.Context, r *connect.Request[kuberov1.ListAlertRulesRequest]) (*connect.Response[kuberov1.ListAlertRulesResponse], error) {
	record(f, "ListAlertRules", r)
	return connect.NewResponse(&kuberov1.ListAlertRulesResponse{Rules: []*kuberov1.AlertRule{
		{Id: "rule-1", Name: "PaymentsErrorRate", Kind: "logs", Query: `sum(rate({namespace="payments",level="error"}[5m]))`, Op: ">", Threshold: 2,
			PendingFor: "5m", Severity: "critical", Enabled: true, Channels: []string{"slack://ops"}}}}), nil
}

func (f *fakeCP) CreateSilence(_ context.Context, r *connect.Request[kuberov1.CreateSilenceRequest]) (*connect.Response[kuberov1.CreateSilenceResponse], error) {
	m := record(f, "CreateSilence", r)
	s := m.GetSilence()
	s.Id = "sil-1"
	return connect.NewResponse(&kuberov1.CreateSilenceResponse{Silence: s}), nil
}

func (f *fakeCP) ListSilences(_ context.Context, r *connect.Request[kuberov1.ListSilencesRequest]) (*connect.Response[kuberov1.ListSilencesResponse], error) {
	record(f, "ListSilences", r)
	return connect.NewResponse(&kuberov1.ListSilencesResponse{Silences: []*kuberov1.Silence{
		{Id: "sil-1", Matchers: map[string]string{"alertname": "PaymentsErrorRate"}, EndsAt: "2026-09-28T14:00:00Z"}}}), nil
}

func (f *fakeCP) DeleteSilence(_ context.Context, r *connect.Request[kuberov1.DeleteSilenceRequest]) (*connect.Response[kuberov1.DeleteSilenceResponse], error) {
	record(f, "DeleteSilence", r)
	return connect.NewResponse(&kuberov1.DeleteSilenceResponse{}), nil
}

func (f *fakeCP) TestAlertRule(_ context.Context, r *connect.Request[kuberov1.TestAlertRuleRequest]) (*connect.Response[kuberov1.TestAlertRuleResponse], error) {
	record(f, "TestAlertRule", r)
	return connect.NewResponse(&kuberov1.TestAlertRuleResponse{Results: []*kuberov1.AlertEvaluation{
		{Labels: map[string]string{"namespace": "payments"}, Value: 140, Triggered: true},
		{Labels: map[string]string{"namespace": "data"}, Value: 3},
	}, ExecMs: 12}), nil
}

// ─── advisor ─────────────────────────────────────────────────────────────

func investigation() *kuberov1.InvestigateResponse {
	return &kuberov1.InvestigateResponse{
		Id: "inv-1", Source: "rules",
		AnswerMarkdown: "### What I found\n\n- checkout-api spend rose 44%.",
		SpokenSummary:  "Checkout's spend rose because of retries.",
		Evidence: []*kuberov1.EvidenceItem{{Kind: "logs", Title: "upstream timeout pattern", Detail: "18,240 lines",
			LinkPath: "/logs?query=x", Query: `{namespace="payments"}`}},
		Actions: []*kuberov1.ProposedAction{{Id: "act-1", Title: "Rightsize checkout-api", Kind: "rightsize.requests",
			Risk: "low", ImpactMonthlyUsd: 6200, CrdYaml: "apiVersion: kubehero.kubehero.io/v1\nkind: RightsizingPolicy\n", Status: "proposed"}},
		Steps: []*kuberov1.InvestigateStep{{Tool: "get_cost_timeseries", Summary: "2 series", DurationMs: 12}},
	}
}

func (f *fakeCP) Investigate(_ context.Context, r *connect.Request[kuberov1.InvestigateRequest]) (*connect.Response[kuberov1.InvestigateResponse], error) {
	record(f, "Investigate", r)
	return connect.NewResponse(investigation()), nil
}

func (f *fakeCP) InvestigateStream(_ context.Context, r *connect.Request[kuberov1.InvestigateStreamRequest], s *connect.ServerStream[kuberov1.InvestigateStreamResponse]) error {
	record(f, "InvestigateStream", r)
	events := []*kuberov1.InvestigateStreamResponse{
		{Event: &kuberov1.InvestigateStreamResponse_Progress{Progress: "Planning the investigation"}},
		{Event: &kuberov1.InvestigateStreamResponse_Step{Step: &kuberov1.InvestigateStep{Tool: "get_cost_timeseries", Summary: "2 series", DurationMs: 12}}},
		{Event: &kuberov1.InvestigateStreamResponse_Step{Step: &kuberov1.InvestigateStep{Tool: "query_logs", Summary: "invalid input", Error: true}}},
		{Event: &kuberov1.InvestigateStreamResponse_Result{Result: investigation()}},
	}
	for _, ev := range events {
		if err := s.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeCP) GetBriefing(_ context.Context, r *connect.Request[kuberov1.GetBriefingRequest]) (*connect.Response[kuberov1.GetBriefingResponse], error) {
	record(f, "GetBriefing", r)
	return connect.NewResponse(&kuberov1.GetBriefingResponse{Briefing: &kuberov1.Briefing{Id: "brf-1", Headline: "$27,700/mo recoverable", Source: "rules"}}), nil
}
