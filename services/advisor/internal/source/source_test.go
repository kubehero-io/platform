// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package source

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
)

var errDown = errors.New("unavailable")

// flaky is the demo backend with selected RPCs failing and a
// configurable origin.
type flaky struct {
	backend.Demo
	origin string
	fail   map[string]bool
}

func (f flaky) Origin() string { return f.origin }

func (f flaky) ListClusters(ctx context.Context, r *kuberov1.ListClustersRequest) (*kuberov1.ListClustersResponse, error) {
	if f.fail["ListClusters"] {
		return nil, errDown
	}
	return f.Demo.ListClusters(ctx, r)
}

func (f flaky) ListWasteRecommendations(ctx context.Context, r *kuberov1.ListWasteRecommendationsRequest) (*kuberov1.ListWasteRecommendationsResponse, error) {
	if f.fail["ListWasteRecommendations"] {
		return nil, errDown
	}
	return f.Demo.ListWasteRecommendations(ctx, r)
}

func (f flaky) ListAnomalies(ctx context.Context, r *kuberov1.ListAnomaliesRequest) (*kuberov1.ListAnomaliesResponse, error) {
	if f.fail["ListAnomalies"] {
		return nil, errDown
	}
	return f.Demo.ListAnomalies(ctx, r)
}

func (f flaky) GetTeamSpend(ctx context.Context, r *kuberov1.GetTeamSpendRequest) (*kuberov1.GetTeamSpendResponse, error) {
	if f.fail["GetTeamSpend"] {
		return nil, errDown
	}
	return f.Demo.GetTeamSpend(ctx, r)
}

func (f flaky) GetAllocation(ctx context.Context, r *kuberov1.GetAllocationRequest) (*kuberov1.GetAllocationResponse, error) {
	if f.fail["GetAllocation"] {
		return nil, errDown
	}
	return f.Demo.GetAllocation(ctx, r)
}

func (f flaky) GetEfficiency(ctx context.Context, r *kuberov1.GetEfficiencyRequest) (*kuberov1.GetEfficiencyResponse, error) {
	if f.fail["GetEfficiency"] {
		return nil, errDown
	}
	return f.Demo.GetEfficiency(ctx, r)
}

func (f flaky) ListRightsizing(ctx context.Context, r *kuberov1.ListRightsizingRequest) (*kuberov1.ListRightsizingResponse, error) {
	if f.fail["ListRightsizing"] {
		return nil, errDown
	}
	return f.Demo.ListRightsizing(ctx, r)
}

func (f flaky) ListAlerts(ctx context.Context, r *kuberov1.ListAlertsRequest) (*kuberov1.ListAlertsResponse, error) {
	if f.fail["ListAlerts"] {
		return nil, errDown
	}
	return f.Demo.ListAlerts(ctx, r)
}

func (f flaky) ListNetworkCosts(ctx context.Context, r *kuberov1.ListNetworkCostsRequest) (*kuberov1.ListNetworkCostsResponse, error) {
	if f.fail["ListNetworkCosts"] {
		return nil, errDown
	}
	return f.Demo.ListNetworkCosts(ctx, r)
}

func (f flaky) GetLogVolume(ctx context.Context, r *kuberov1.GetLogVolumeRequest) (*kuberov1.GetLogVolumeResponse, error) {
	if f.fail["GetLogVolume"] {
		return nil, errDown
	}
	return f.Demo.GetLogVolume(ctx, r)
}

func TestDemoSnapshotIsEnriched(t *testing.T) {
	snap := DemoSnapshot()
	if snap.Origin != "demo" || len(snap.Clusters) != 2 || len(snap.Waste) != 3 || len(snap.Anomalies) != 2 {
		t.Fatalf("core fixture changed: %+v", snap)
	}
	if snap.BurnRate == nil || snap.BurnRate.BurnRateMilli != 1350 {
		t.Errorf("burn rate = %+v", snap.BurnRate)
	}
	if len(snap.CostAllocation) == 0 || snap.CostAllocation[0].Namespace != "ml-inference" {
		t.Errorf("cost allocation = %+v", snap.CostAllocation)
	}
	if snap.Efficiency == nil || snap.Efficiency.Score != 41 {
		t.Errorf("efficiency = %+v", snap.Efficiency)
	}
	if len(snap.Rightsizing) == 0 || snap.Rightsizing[0].Workload != "checkout-api" {
		t.Errorf("rightsizing (largest saving first) = %+v", snap.Rightsizing)
	}
	if len(snap.FiringAlerts) != 2 || snap.FiringAlerts[0].Severity != "critical" || snap.FiringAlerts[0].Namespace != "payments" {
		t.Errorf("alerts (critical first) = %+v", snap.FiringAlerts)
	}
	if len(snap.NetworkTop) == 0 || snap.NetworkTop[0].Workload != "api-gateway" {
		t.Errorf("network = %+v", snap.NetworkTop)
	}
	if snap.LogErrors == nil || len(snap.LogErrors.ByNamespace) == 0 {
		t.Fatalf("log errors = %+v", snap.LogErrors)
	}
	top := snap.LogErrors.ByNamespace[0]
	if top.Namespace != "payments" || top.SpikeRatio < 3 {
		t.Errorf("payments error spike not detected: %+v", top)
	}
	// The snapshot is prompt input: it must always marshal.
	if _, err := json.Marshal(snap); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichmentFailuresAreSkipped(t *testing.T) {
	b := flaky{origin: "control-plane", fail: map[string]bool{
		"GetAllocation": true, "GetEfficiency": true, "ListRightsizing": true,
		"ListAlerts": true, "ListNetworkCosts": true, "GetLogVolume": true,
	}}
	snap, err := NewControlPlane(b).Fetch(context.Background(), "", "24h")
	if err != nil {
		t.Fatalf("optional enrichment failing must not fail the snapshot: %v", err)
	}
	if snap.Origin != "control-plane" || len(snap.Waste) == 0 || snap.Spend == nil {
		t.Errorf("core signals missing: %+v", snap)
	}
	if snap.CostAllocation != nil || snap.Efficiency != nil || snap.Rightsizing != nil ||
		snap.FiringAlerts != nil || snap.NetworkTop != nil || snap.LogErrors != nil {
		t.Errorf("failed enrichment leaked into the snapshot: %+v", snap)
	}
}

func TestCoreFailureMeansUnreachable(t *testing.T) {
	b := flaky{origin: "control-plane", fail: map[string]bool{
		"ListClusters": true, "ListWasteRecommendations": true, "ListAnomalies": true, "GetTeamSpend": true,
	}}
	if _, err := NewControlPlane(b).Fetch(context.Background(), "", "24h"); err == nil {
		t.Fatal("want an error when every core signal fails")
	}
}

func TestLiveSnapshotDropsDemoLabelledEnrichment(t *testing.T) {
	// A live control plane whose cost engine still serves demo fixtures
	// must not have them presented as real numbers.
	snap, err := NewControlPlane(flaky{origin: "control-plane"}).Fetch(context.Background(), "", "24h")
	if err != nil {
		t.Fatal(err)
	}
	if snap.CostAllocation != nil || snap.Efficiency != nil || snap.Rightsizing != nil || snap.NetworkTop != nil {
		t.Errorf("demo-labelled enrichment kept in a live snapshot: %+v", snap)
	}
	if len(snap.FiringAlerts) == 0 {
		t.Error("unlabelled enrichment (alerts) should still be kept")
	}
}

func pts(vals ...float64) []*kuberov1.Point {
	out := make([]*kuberov1.Point, len(vals))
	for i, v := range vals {
		out[i] = &kuberov1.Point{TsUnixMs: int64(i), Value: v}
	}
	return out
}

func TestSeriesShift(t *testing.T) {
	flat := make([]float64, 48)
	jump := make([]float64, 48)
	for i := range flat {
		flat[i] = 10
		jump[i] = 10
		if i >= 42 {
			jump[i] = 20 // last 6 points doubled
		}
	}
	tests := []struct {
		name      string
		points    []*kuberov1.Point
		wantPct   float64
		wantPts   int
		wantRatio float64
	}{
		{"flat", pts(flat...), 0, 24, 1},
		{"recent jump found at its own width", pts(jump...), 100, 6, 2},
		{"too short", pts(1, 2, 3), 0, 0, 0},
		{"zero baseline caps", pts(0, 0, 0, 0, 0, 0, 5, 5), 0, 3, maxSpikeRatio},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sh := SeriesShift(tc.points)
			if int(sh.ChangePct+0.5) != int(tc.wantPct) || sh.RecentPoints != tc.wantPts || sh.Ratio() != tc.wantRatio {
				t.Errorf("got change %.1f%% over %d pts ratio %v, want %.0f%% over %d ratio %v",
					sh.ChangePct, sh.RecentPoints, sh.Ratio(), tc.wantPct, tc.wantPts, tc.wantRatio)
			}
		})
	}
}
