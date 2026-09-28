// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package backend_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/backend/backendtest"
)

func TestConnectSendsBearerToken(t *testing.T) {
	srv := backendtest.New(t, backend.Demo{}, "s3cret")
	ctx := context.Background()

	ok := backend.NewConnect(srv.URL+"/", "s3cret")
	res, err := ok.ListClusters(ctx, &kuberov1.ListClustersRequest{})
	if err != nil {
		t.Fatalf("with token: %v", err)
	}
	if len(res.GetClusters()) != 2 {
		t.Fatalf("clusters = %d", len(res.GetClusters()))
	}

	anon := backend.NewConnect(srv.URL, "")
	if _, err := anon.ListClusters(ctx, &kuberov1.ListClustersRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("without token: err = %v, want unauthenticated", err)
	}
}

// TestConnectRoundTripsEveryRPC drives every Backend method through the
// generated client and handlers, so a wiring slip (wrong service, wrong
// method) fails here rather than in production.
func TestConnectRoundTripsEveryRPC(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	srv := backendtest.New(t, backend.Demo{Now: func() time.Time { return now }}, "")
	c := backend.NewConnect(srv.URL, "")
	ctx := context.Background()
	start, end := now.Add(-time.Hour).UnixMilli(), now.UnixMilli()
	sel := &kuberov1.ProfileSelector{Service: "checkout-api"}

	checks := []struct {
		method string
		call   func() (int, error)
	}{
		{"ListWasteRecommendations", func() (int, error) {
			r, err := c.ListWasteRecommendations(ctx, &kuberov1.ListWasteRecommendationsRequest{})
			return len(r.GetRecommendations()), err
		}},
		{"ListAnomalies", func() (int, error) {
			r, err := c.ListAnomalies(ctx, &kuberov1.ListAnomaliesRequest{})
			return len(r.GetAnomalies()), err
		}},
		{"GetTeamSpend", func() (int, error) {
			r, err := c.GetTeamSpend(ctx, &kuberov1.GetTeamSpendRequest{})
			return len(r.GetTeams()), err
		}},
		{"GetBurnRate", func() (int, error) {
			r, err := c.GetBurnRate(ctx, &kuberov1.GetBurnRateRequest{Window: "24h"})
			return int(r.GetBurnRateMilli()), err
		}},
		{"GetWorkload", func() (int, error) {
			r, err := c.GetWorkload(ctx, &kuberov1.GetWorkloadRequest{Namespace: "payments", Name: "checkout-api"})
			return len(r.GetHistory()), err
		}},
		{"ListCapacityDemands", func() (int, error) {
			r, err := c.ListCapacityDemands(ctx, &kuberov1.ListCapacityDemandsRequest{})
			return len(r.GetDemands()), err
		}},
		{"GetAllocation", func() (int, error) {
			r, err := c.GetAllocation(ctx, &kuberov1.GetAllocationRequest{Window: "7d", Aggregate: []string{"namespace"}})
			return len(r.GetAllocations()), err
		}},
		{"GetCostTimeseries", func() (int, error) {
			r, err := c.GetCostTimeseries(ctx, &kuberov1.GetCostTimeseriesRequest{Window: "7d", GroupBy: "namespace"})
			return len(r.GetSeries()), err
		}},
		{"ListRightsizing", func() (int, error) {
			r, err := c.ListRightsizing(ctx, &kuberov1.ListRightsizingRequest{})
			return len(r.GetRecommendations()), err
		}},
		{"GetEfficiency", func() (int, error) {
			r, err := c.GetEfficiency(ctx, &kuberov1.GetEfficiencyRequest{})
			return len(r.GetNamespaces()), err
		}},
		{"QueryLogs", func() (int, error) {
			r, err := c.QueryLogs(ctx, &kuberov1.QueryLogsRequest{Query: `{namespace="payments"}`, StartUnixMs: start, EndUnixMs: end, Limit: 5})
			return len(r.GetLines()), err
		}},
		{"GetLogVolume", func() (int, error) {
			r, err := c.GetLogVolume(ctx, &kuberov1.GetLogVolumeRequest{Query: `{level="error"}`, StartUnixMs: start, EndUnixMs: end})
			return len(r.GetSeries()), err
		}},
		{"GetLogPatterns", func() (int, error) {
			r, err := c.GetLogPatterns(ctx, &kuberov1.GetLogPatternsRequest{Query: `{namespace="payments"}`})
			return len(r.GetPatterns()), err
		}},
		{"ListProfileTargets", func() (int, error) {
			r, err := c.ListProfileTargets(ctx, &kuberov1.ListProfileTargetsRequest{})
			return len(r.GetTargets()), err
		}},
		{"GetFlamegraph", func() (int, error) {
			r, err := c.GetFlamegraph(ctx, &kuberov1.GetFlamegraphRequest{Selector: sel})
			return len(r.GetNodes()), err
		}},
		{"GetTopFunctions", func() (int, error) {
			r, err := c.GetTopFunctions(ctx, &kuberov1.GetTopFunctionsRequest{Selector: sel, Limit: 3})
			return len(r.GetFunctions()), err
		}},
		{"GetServiceMap", func() (int, error) {
			r, err := c.GetServiceMap(ctx, &kuberov1.GetServiceMapRequest{})
			return len(r.GetEdges()), err
		}},
		{"ListNetworkCosts", func() (int, error) {
			r, err := c.ListNetworkCosts(ctx, &kuberov1.ListNetworkCostsRequest{})
			return len(r.GetCosts()), err
		}},
		{"ListAlerts", func() (int, error) {
			r, err := c.ListAlerts(ctx, &kuberov1.ListAlertsRequest{State: "firing"})
			return len(r.GetAlerts()), err
		}},
	}
	for _, tc := range checks {
		t.Run(tc.method, func(t *testing.T) {
			n, err := tc.call()
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				t.Errorf("%s returned nothing through the wire", tc.method)
			}
			if srv.Calls(tc.method) != 1 {
				t.Errorf("%s hit %d times, want 1", tc.method, srv.Calls(tc.method))
			}
		})
	}
}

func TestDemoIsLabelledAndCoherent(t *testing.T) {
	d := backend.Demo{Now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }}
	ctx := context.Background()
	if d.Origin() != "demo" {
		t.Fatalf("origin = %q", d.Origin())
	}
	alloc, _ := d.GetAllocation(ctx, &kuberov1.GetAllocationRequest{Window: "30d", Aggregate: []string{"namespace", "workload"}})
	if alloc.GetSource() != "demo" {
		t.Errorf("allocation source = %q", alloc.GetSource())
	}
	// 30 days ≈ one month: allocation totals should land near the fleet
	// total the team-spend fixture reports.
	if got := alloc.GetTotals().GetTotalCost(); got < 120000 || got > 130000 {
		t.Errorf("30d allocation total = %.0f, want ~126k (fleet $128.4k/mo)", got)
	}
	var sawCheckout bool
	for _, a := range alloc.GetAllocations() {
		if a.GetName() == "payments/checkout-api" && a.GetProperties()["workload"] == "checkout-api" {
			sawCheckout = true
		}
	}
	if !sawCheckout {
		t.Error("composite namespace/workload key missing for checkout-api")
	}
	rs, _ := d.ListRightsizing(ctx, &kuberov1.ListRightsizingRequest{Namespace: "payments"})
	if len(rs.GetRecommendations()) != 1 || rs.GetSource() != "demo" {
		t.Errorf("namespace filter / label broken: %+v", rs)
	}
}
