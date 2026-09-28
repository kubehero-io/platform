// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

var fixedNow = time.Date(2026, 9, 30, 14, 25, 0, 0, time.UTC)

func viewer() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Sub: "t", Role: auth.RoleViewer})
}

func demoService() *Service { return &Service{Now: func() time.Time { return fixedNow }} }

func code(err error) connect.Code {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code()
	}
	return connect.CodeUnknown
}

func alloc(t *testing.T, s *Service, m *kuberov1.GetAllocationRequest) *kuberov1.GetAllocationResponse {
	t.Helper()
	r, err := s.GetAllocation(viewer(), connect.NewRequest(m))
	if err != nil {
		t.Fatalf("GetAllocation(%v): %v", m, err)
	}
	return r.Msg
}

func sumRows(r *kuberov1.GetAllocationResponse) (total float64) {
	for _, a := range r.GetAllocations() {
		total += a.GetTotalCost()
	}
	return total
}

func TestGetAllocationDemoInvariants(t *testing.T) {
	s := demoService()
	base := &kuberov1.GetAllocationRequest{Window: "yesterday", Aggregate: []string{"namespace"}, IncludeIdle: true}
	sep := alloc(t, s, base)
	if sep.GetSource() != SourceDemo {
		t.Fatalf("source = %q", sep.GetSource())
	}
	if !near(sep.GetTotals().GetTotalCost(), sumRows(sep)) {
		t.Fatalf("totals %v != Σ rows %v", sep.GetTotals().GetTotalCost(), sumRows(sep))
	}
	idleRows := 0
	for _, a := range sep.GetAllocations() {
		if a.GetIdleCost() > 0 {
			idleRows++
			if a.GetProperties()["cluster"] == "" {
				t.Errorf("idle row %s without cluster property", a.GetName())
			}
		}
	}
	if idleRows != 3 {
		t.Fatalf("want one idle row per demo cluster, got %d", idleRows)
	}

	// Sharing idle or shared namespaces moves dollars between rows but
	// never creates or destroys them.
	for _, variant := range []*kuberov1.GetAllocationRequest{
		{Window: "yesterday", Aggregate: []string{"namespace"}, ShareIdle: "weighted"},
		{Window: "yesterday", Aggregate: []string{"namespace"}, ShareIdle: "even"},
		{Window: "yesterday", Aggregate: []string{"team"}, IncludeIdle: true, SharedNamespaces: []string{"kube-system", "monitoring"}},
		{Window: "yesterday", Aggregate: []string{"cluster", "controller"}, IncludeIdle: true},
		{Window: "yesterday", Aggregate: []string{"pod"}, IncludeIdle: true},
		{Window: "yesterday", Aggregate: []string{"container"}, IncludeIdle: true},
		{Window: "yesterday", Aggregate: []string{"label:app"}, IncludeIdle: true},
	} {
		got := alloc(t, s, variant)
		if !near(got.GetTotals().GetTotalCost(), sep.GetTotals().GetTotalCost()) {
			t.Errorf("%v: total %v, want %v", variant, got.GetTotals().GetTotalCost(), sep.GetTotals().GetTotalCost())
		}
	}

	shared := alloc(t, s, &kuberov1.GetAllocationRequest{Window: "yesterday", Aggregate: []string{"namespace"},
		SharedNamespaces: []string{"kube-system"}})
	for _, a := range shared.GetAllocations() {
		if a.GetName() == "kube-system" {
			t.Fatal("shared namespace listed as its own row")
		}
	}

	// A filter shows the filtered rows and only their share of idle.
	f := alloc(t, s, &kuberov1.GetAllocationRequest{Window: "yesterday", Aggregate: []string{"workload"},
		Filters: map[string]string{"namespace": "checkout"}, IncludeIdle: true})
	for _, a := range f.GetAllocations() {
		if a.GetIdleCost() == 0 && a.GetName() != "cart" && a.GetName() != "checkout" {
			t.Fatalf("filter leaked %q", a.GetName())
		}
	}
	if f.GetTotals().GetTotalCost() >= sep.GetTotals().GetTotalCost()/5 {
		t.Fatal("filtered total should be a small slice of the fleet")
	}

	multi := alloc(t, s, &kuberov1.GetAllocationRequest{Window: "yesterday", Aggregate: []string{"namespace"},
		Filters: map[string]string{"namespace": "checkout,edge"}})
	if len(multi.GetAllocations()) != 2 {
		t.Fatalf("comma filter = %d rows", len(multi.GetAllocations()))
	}
}

func TestGetAllocationValidationAndAuth(t *testing.T) {
	s := demoService()
	for _, m := range []*kuberov1.GetAllocationRequest{
		{Window: "nonsense"},
		{Aggregate: []string{"service"}},
		{ShareIdle: "maybe"},
		{Filters: map[string]string{"bogus": "x"}},
	} {
		if _, err := s.GetAllocation(viewer(), connect.NewRequest(m)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: want InvalidArgument, got %v", m, err)
		}
	}
	if _, err := s.GetAllocation(context.Background(), connect.NewRequest(&kuberov1.GetAllocationRequest{})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous must be denied, got %v", err)
	}
	off := &Service{DemoDisabled: true, Now: func() time.Time { return fixedNow }}
	if _, err := off.GetAllocation(viewer(), connect.NewRequest(&kuberov1.GetAllocationRequest{})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("demo disabled must fail loudly, got %v", err)
	}
	tok := auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleMember, ClusterID: "eks-use1-prod"})
	r, err := s.GetAllocation(tok, connect.NewRequest(&kuberov1.GetAllocationRequest{Window: "yesterday", Aggregate: []string{"cluster"}}))
	if err != nil || len(r.Msg.GetAllocations()) != 1 || r.Msg.GetAllocations()[0].GetName() != "eks-use1-prod" {
		t.Fatalf("cluster token must be scoped to its cluster: %v %v", err, r)
	}
	if _, err := s.GetAllocation(tok, connect.NewRequest(&kuberov1.GetAllocationRequest{ClusterId: "gke-usc1-prod"})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("cluster token reading another cluster: %v", err)
	}
}

func TestGetCostTimeseriesDemo(t *testing.T) {
	s := demoService()
	r, err := s.GetCostTimeseries(viewer(), connect.NewRequest(&kuberov1.GetCostTimeseriesRequest{
		Window: "7d", GroupBy: "namespace", Top: 3}))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Msg
	if len(m.GetSeries()) != 4 || m.GetSeries()[3].GetLabels()["name"] != otherName {
		t.Fatalf("want top 3 + other, got %d series", len(m.GetSeries()))
	}
	var sum float64
	for _, s := range m.GetSeries() {
		if len(s.GetPoints()) != 7 {
			t.Fatalf("7d daily series must have 7 dense points, got %d", len(s.GetPoints()))
		}
		for _, p := range s.GetPoints() {
			sum += p.GetValue()
		}
	}
	if !near(sum, m.GetTotalUsd()) || m.GetTotalUsd() <= 0 {
		t.Fatalf("Σ points %v != total %v", sum, m.GetTotalUsd())
	}
	if m.GetForecastMonthUsd() <= 0 {
		t.Fatal("forecast must be positive")
	}
	pts := m.GetSeries()[0].GetPoints()
	if time.UnixMilli(pts[0].GetTsUnixMs()).UTC().Hour() != 0 {
		t.Fatal("daily points must sit on midnight UTC")
	}

	hourly, err := s.GetCostTimeseries(viewer(), connect.NewRequest(&kuberov1.GetCostTimeseriesRequest{Window: "24h"}))
	if err != nil || len(hourly.Msg.GetSeries()) != 1 || len(hourly.Msg.GetSeries()[0].GetPoints()) < 24 {
		t.Fatalf("auto step for 24h must be hourly: %v", err)
	}
	if _, err := s.GetCostTimeseries(viewer(), connect.NewRequest(&kuberov1.GetCostTimeseriesRequest{GroupBy: "pod"})); code(err) != connect.CodeInvalidArgument {
		t.Fatalf("pod grouping must be rejected: %v", err)
	}
}

func TestListRightsizingDemo(t *testing.T) {
	s := demoService()
	r, err := s.ListRightsizing(viewer(), connect.NewRequest(&kuberov1.ListRightsizingRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	recs := r.Msg.GetRecommendations()
	if len(recs) == 0 || r.Msg.GetSource() != SourceDemo {
		t.Fatal("demo recommendations expected")
	}
	for i := 1; i < len(recs); i++ {
		if recs[i].GetSavingsUsdMonth() > recs[i-1].GetSavingsUsdMonth() {
			t.Fatal("must be sorted by savings desc")
		}
	}
	lim, _ := s.ListRightsizing(viewer(), connect.NewRequest(&kuberov1.ListRightsizingRequest{Limit: 2, MinSavingsUsdMonth: 1}))
	if len(lim.Msg.GetRecommendations()) != 2 {
		t.Fatal("limit")
	}
	for _, bad := range []*kuberov1.ListRightsizingRequest{{HeadroomPct: -1}, {CpuPercentile: "p50"}, {Window: "120d"}, {MinSavingsUsdMonth: -5}} {
		if _, err := s.ListRightsizing(viewer(), connect.NewRequest(bad)); code(err) != connect.CodeInvalidArgument {
			t.Errorf("%v: want InvalidArgument, got %v", bad, err)
		}
	}
}

func TestGetEfficiencyDemo(t *testing.T) {
	s := demoService()
	r, err := s.GetEfficiency(viewer(), connect.NewRequest(&kuberov1.GetEfficiencyRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	m := r.Msg
	if m.GetScore() <= 0 || m.GetScore() >= 100 || len(m.GetClusters()) != 3 || len(m.GetNamespaces()) < 5 {
		t.Fatalf("efficiency: score %v, %d clusters, %d namespaces", m.GetScore(), len(m.GetClusters()), len(m.GetNamespaces()))
	}
	if m.GetIdleCostUsdMonth() <= 0 || m.GetRecoverableUsdMonth() <= 0 {
		t.Fatalf("idle %v recoverable %v", m.GetIdleCostUsdMonth(), m.GetRecoverableUsdMonth())
	}
}

func TestScoreFormula(t *testing.T) {
	m := metrics{Cost: 100, CPUCost: 60, RAMCost: 40, CPUReqCS: 10, CPUUseCS: 5, RAMReqBS: 10, RAMUseBS: 20}
	// blend = (0.5·60 + 1·40)/100 = 0.7 (RAM over-use capped at 1);
	// idle share = 25/125 = 0.2 → 100 · 0.7 · 0.8 = 56.
	if got := Score(&m, 25); !near(got, 56) {
		t.Fatalf("Score = %v, want 56", got)
	}
	if Score(&metrics{}, 0) != 0 {
		t.Fatal("empty scope scores 0")
	}
}
