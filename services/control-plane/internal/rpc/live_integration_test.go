// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

// Needs both test stores:
//
//	KUBEHERO_TEST_CLICKHOUSE_URL=clickhouse://kubehero:kubehero@localhost:19000/kubehero
//	KUBEHERO_TEST_POSTGRES_URL=postgres://kubehero:kubehero@localhost:15432/kubehero?sslmode=disable
package rpc

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/chfixture"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/pgtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

var podCostCols = []string{"ts", "org_id", "cluster_id", "node", "namespace", "pod", "team", "cost_center", "nodepool",
	"cloud", "region", "sku", "lifecycle", "gpu_kind", "cpu_millicores", "mem_bytes", "gpu_util_pct", "cost_usd_sec",
	"recoverable_usd_sec", "interval_sec", "workload", "workload_kind", "zone", "cpu_usage_millicores",
	"mem_usage_bytes", "cpu_cost_usd_sec", "ram_cost_usd_sec"}

func seedLive(t *testing.T, ch *sql.DB, clusterID string, now time.Time) float64 {
	t.Helper()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	from := now.Add(-2 * time.Hour)
	if from.Before(monthStart) {
		from = monthStart
	}
	var cost [][]any
	shopMTD := 0.0
	for ts := from; ts.Before(now.Add(-time.Minute)); ts = ts.Add(time.Minute) {
		for _, p := range []struct{ wl, pod, team string }{
			{"api", "api-7d9f8c6b5-bcdfg", "payments"}, {"api", "api-7d9f8c6b5-hjklm", "payments"}, {"cache", "cache-0", "payments"},
		} {
			cost = append(cost, []any{ts.UnixMilli(), "default", clusterID, "n1", "shop", p.pod, p.team, "cc1", "general", "aws",
				"us-east-1", "m7i.xlarge", "on-demand", "", uint32(2000), uint64(4 << 30), float32(0), 0.0002, 0.0001,
				float32(60), p.wl, "Deployment", "z1", uint32(300), uint64(1 << 30), 0.00012, 0.00008})
			shopMTD += 0.0002 * 60
		}
	}
	chfixture.Insert(t, ch, "pod_cost_1s", podCostCols, cost)

	var nodes [][]any
	for ts := now.Add(-10 * time.Minute); ts.Before(now); ts = ts.Add(time.Minute) {
		for _, n := range []string{"n1", "n2", "n3"} {
			nodes = append(nodes, []any{ts.UnixMilli(), "default", clusterID, n, "general", "aws", "us-east-1", "z1", "m7i.xlarge",
				"on-demand", "", uint8(0), float32(60), 0.2016, "test", uint32(4000), uint64(16 << 30), uint32(0), uint64(0),
				uint32(0), uint64(0), 0.2016 / 3600, 0.0})
		}
		nodes = append(nodes, []any{ts.UnixMilli(), "default", "rogue", "r1", "general", "gcp", "us-central1", "a", "n2-standard-4",
			"on-demand", "", uint8(0), float32(60), 0.19, "test", uint32(4000), uint64(16 << 30), uint32(0), uint64(0),
			uint32(0), uint64(0), 0.19 / 3600, 0.0})
	}
	chfixture.Insert(t, ch, "node_cost_1s", []string{"ts", "org_id", "cluster_id", "node", "nodepool", "cloud", "region",
		"zone", "sku", "lifecycle", "gpu_kind", "gpu_count", "interval_sec", "price_per_hour", "price_source",
		"cpu_allocatable_millicores", "mem_allocatable_bytes", "cpu_requested_millicores", "mem_requested_bytes",
		"cpu_used_millicores", "mem_used_bytes", "cost_usd_sec", "idle_usd_sec"}, nodes)

	// Two days of usage: api requests 2 cores but uses ~0.3.
	var usage [][]any
	i := 0
	for ts := now.Add(-48 * time.Hour); ts.Before(now); ts = ts.Add(5 * time.Minute) {
		cpu := float32(0.2 + 0.1*float64(i%10)/9)
		i++
		usage = append(usage, []any{ts, "default", clusterID, "shop", "api", "Deployment", "api-7d9f8c6b5-bcdfg", "app", "n1",
			"payments", cpu, uint64(600 << 20), float32(2), uint64(4 << 30), float32(0), uint64(0), uint32(0), ""})
	}
	chfixture.Insert(t, ch, "container_usage", []string{"ts", "org_id", "cluster_id", "namespace", "workload", "workload_kind",
		"pod", "container", "node", "team", "cpu_usage_cores", "mem_working_set_bytes", "cpu_request_cores",
		"mem_request_bytes", "cpu_limit_cores", "mem_limit_bytes", "restarts", "last_termination_reason"}, usage)

	ev := func(ts time.Time, kind, wl, pod, container string, attrs map[string]string) []any {
		return []any{ts, "default", clusterID, kind, "warn", "shop", wl, pod, container, "", "", "0/3 nodes are available", attrs, uint32(1)}
	}
	big := map[string]string{"cpu_millicores": "3000", "mem_bytes": "8589934592", "age_sec": "600"}
	events := [][]any{
		ev(now.Add(-5*time.Minute), "unschedulable", "etl", "etl-6c8d7f9b5-aaaaa", "", big),
		ev(now.Add(-5*time.Minute), "unschedulable", "etl", "etl-6c8d7f9b5-bbbbb", "", big),
		ev(now.Add(-5*time.Minute), "unschedulable", "etl", "etl-6c8d7f9b5-ccccc", "", big),
		// Placed since: it shows up in pod_cost_1s after its event.
		ev(now.Add(-20*time.Minute), "unschedulable", "etl", "etl-6c8d7f9b5-ddddd", "", big),
	}
	for k := 0; k < 4; k++ {
		events = append(events, ev(now.Add(-time.Duration(10+k*5)*time.Minute), "oom_killed", "cache", "cache-0", "redis", map[string]string{}))
	}
	chfixture.Insert(t, ch, "cluster_events", []string{"ts", "org_id", "cluster_id", "kind", "severity", "namespace",
		"workload", "pod", "container", "node", "reason", "message", "attributes", "count"}, events)
	chfixture.Insert(t, ch, "pod_cost_1s", podCostCols, [][]any{{now.Add(-10 * time.Minute).UnixMilli(), "default", clusterID, "n2",
		"shop", "etl-6c8d7f9b5-ddddd", "data", "", "general", "aws", "us-east-1", "m7i.xlarge", "on-demand", "", uint32(3000),
		uint64(8 << 30), float32(0), 0.0, 0.0, float32(60), "etl", "Deployment", "z1", uint32(0), uint64(0), 0.0, 0.0}})

	// Error logs: ~100/h for a day, then 1000 in the last hour.
	var logs [][]any
	for h := 25; h >= 1; h-- {
		lines := uint64(95 + (h%3)*5)
		if h == 1 {
			lines = 1000
		}
		logs = append(logs, []any{now.Add(-time.Duration(h) * time.Hour).Add(30 * time.Minute).Truncate(time.Minute),
			"default", clusterID, "shop", "api", "app", "payments", "error", lines, lines * 120})
	}
	chfixture.Insert(t, ch, "log_volume_1m", []string{"ts_minute", "org_id", "cluster_id", "namespace", "workload",
		"container", "team", "level", "lines", "bytes"}, logs)
	return shopMTD
}

func TestControlPlaneLiveRPCs(t *testing.T) {
	ch := chfixture.Open(t, "rpc")
	pg := pgtest.Open(t, "rpc")
	ctx := context.Background()
	now := time.Now().UTC()

	clustersPG := &store.ClustersPG{DB: pg}
	reg := &store.Cluster{OrgID: "default", Slug: "prod", Name: "Prod", Cloud: "aws", Region: "us-east-1", State: "healthy"}
	if err := clustersPG.Register(ctx, reg); err != nil {
		t.Fatal(err)
	}
	policies := &store.PoliciesPG{DB: pg}
	for _, p := range []*store.Policy{
		{ClusterID: reg.ID, Kind: "BudgetPolicy", Namespace: "kubehero-system", Name: "shop-budget",
			SpecJSON: []byte(`{"ceiling":"$10/mo","scope":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"shop"}}}}`)},
		{ClusterID: reg.ID, Kind: "CeilingPolicy", Namespace: "kubehero-system", Name: "shop-ceiling",
			SpecJSON: []byte(`{"budgetRef":"shop-budget","trigger":{"burnRateMilli":1500,"window":"5m"}}`)},
	} {
		if err := policies.Upsert(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	audit := &store.AuditPG{DB: pg}
	if _, err := audit.Append(ctx, &store.AuditEntry{At: now, OrgID: strPtrAudit("default"), ClusterID: strPtrAudit("prod"),
		ActorSub: "operator", Action: "rightsize.shadow", TargetKind: "Deployment", TargetName: "api", Outcome: "armed"}); err != nil {
		t.Fatal(err)
	}
	shopMTD := seedLive(t, ch, reg.ID, now)

	cp := New(Options{Clusters: clustersPG, Policies: policies, Audit: audit, PG: pg, CH: ch,
		Anomalies: &clickhouse.SpendAnomalyProvider{DB: ch}, DemoFixturesDisabled: true})

	t.Run("ListClusters", func(t *testing.T) {
		r, err := cp.ListClusters(ctx, connect.NewRequest(&kuberov1.ListClustersRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]*kuberov1.Cluster{}
		for _, c := range r.Msg.GetClusters() {
			got[c.GetId()] = c
		}
		if c := got[reg.ID]; c == nil || c.GetNodes() != 3 || c.GetName() != "Prod" {
			t.Fatalf("registered cluster: %+v", c)
		}
		if c := got["rogue"]; c == nil || c.GetNodes() != 1 || c.GetCloud() != "gcp" {
			t.Fatalf("reporting-but-unregistered cluster: %+v", c)
		}
	})

	t.Run("ListWasteRecommendations + GetWorkload", func(t *testing.T) {
		r, err := cp.ListWasteRecommendations(ctx, connect.NewRequest(&kuberov1.ListWasteRecommendationsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		recs := r.Msg.GetRecommendations()
		if len(recs) == 0 || recs[0].GetWorkload() != "api" || recs[0].GetCluster() != "prod" || recs[0].GetCloud() != "EKS" {
			t.Fatalf("waste: %+v", recs)
		}
		if recs[0].GetRecoverableUsdMonth() <= 0 || !strings.HasPrefix(recs[0].GetSignal(), "cpu.req=2.0 p95=") {
			t.Fatalf("waste rec: %+v", recs[0])
		}
		w, err := cp.GetWorkload(ctx, connect.NewRequest(&kuberov1.GetWorkloadRequest{Cluster: "prod", Namespace: "shop", Name: "api"}))
		if err != nil {
			t.Fatal(err)
		}
		if w.Msg.GetRecommendation() == nil || len(w.Msg.GetHistory()) != 1 || w.Msg.GetHistory()[0].GetAction() != "rightsize.shadow" {
			t.Fatalf("workload: rec %v history %v", w.Msg.GetRecommendation(), w.Msg.GetHistory())
		}
	})

	t.Run("ListPolicies", func(t *testing.T) {
		r, err := cp.ListPolicies(ctx, connect.NewRequest(&kuberov1.ListPoliciesRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		ps := r.Msg.GetPolicies()
		if len(ps) != 2 {
			t.Fatalf("policies: %+v", ps)
		}
		wantPct := int32(math.Round(shopMTD / 10 * 100))
		for _, p := range ps {
			if p.GetCeilingUsd() != 10 || p.GetSpentPct() != wantPct {
				t.Errorf("%s: ceiling %v spent %d%%, want 10 / %d%%", p.GetName(), p.GetCeilingUsd(), p.GetSpentPct(), wantPct)
			}
			if !strings.Contains(p.GetScope(), "ns=shop") || !strings.Contains(p.GetScope(), "prod") {
				t.Errorf("%s scope %q", p.GetName(), p.GetScope())
			}
		}
		only, _ := cp.ListPolicies(ctx, connect.NewRequest(&kuberov1.ListPoliciesRequest{Kind: "CeilingPolicy", ClusterId: "prod"}))
		if len(only.Msg.GetPolicies()) != 1 || !strings.HasPrefix(only.Msg.GetPolicies()[0].GetScope(), "budget shop-budget") {
			t.Fatalf("filtered: %+v", only.Msg.GetPolicies())
		}
	})

	t.Run("ListCapacityDemands", func(t *testing.T) {
		r, err := cp.ListCapacityDemands(ctx, connect.NewRequest(&kuberov1.ListCapacityDemandsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		ds := r.Msg.GetDemands()
		if len(ds) != 1 {
			t.Fatalf("demands: %+v", ds)
		}
		d := ds[0]
		if d.GetWorkload() != "etl" || d.GetPendingPods() != 3 || d.GetRequestedCpu() != "9 cores" || d.GetRequestedMem() != "24 GiB" {
			t.Fatalf("demand: %+v", d)
		}
		if !strings.Contains(d.GetRecommendedAction(), "by 3 nodes (m7i.xlarge)") ||
			math.Abs(d.GetRecommendedCostUsdMonth()-3*0.2016*720) > 0.01 || math.Abs(d.GetBlockedCostUsdMonth()-9*0.2016/4*720) > 0.01 {
			t.Fatalf("pricing: %+v", d)
		}
		if r.Msg.GetTotalPendingPods() != 3 {
			t.Fatalf("totals: %d", r.Msg.GetTotalPendingPods())
		}
	})

	t.Run("ListAnomalies adds OOM bursts and log spikes", func(t *testing.T) {
		r, err := cp.ListAnomalies(ctx, connect.NewRequest(&kuberov1.ListAnomaliesRequest{Limit: 20}))
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]*kuberov1.Anomaly{}
		for _, a := range r.Msg.GetAnomalies() {
			kinds[a.GetKind()] = a
		}
		if a := kinds["capacity"]; a == nil || !strings.Contains(a.GetTitle(), "4 OOM kills") || a.GetImpactUsdMonth() <= 0 {
			t.Fatalf("oom burst: %+v", a)
		}
		if a := kinds["logs"]; a == nil || a.GetSubject() != "shop/api" || !strings.Contains(a.GetLinkPath(), "/logs?query=") {
			t.Fatalf("log spike: %+v", a)
		}
	})

	t.Run("GetTeamSpend", func(t *testing.T) {
		r, err := cp.GetTeamSpend(ctx, connect.NewRequest(&kuberov1.GetTeamSpendRequest{Window: "24h"}))
		if err != nil {
			t.Fatal(err)
		}
		var payments *kuberov1.TeamSpend
		for _, tm := range r.Msg.GetTeams() {
			if tm.GetTeam() == "payments" {
				payments = tm
			}
		}
		if payments == nil || payments.GetSpendUsdMonth() <= 0 || payments.GetAwsUsdMonth() != payments.GetSpendUsdMonth() ||
			payments.GetRecoverableUsdMonth() <= 0 {
			t.Fatalf("payments: %+v", payments)
		}
		// ~2h of shop spend scaled from a 24h window to 30 days.
		want := shopMTD * 30
		if math.Abs(payments.GetSpendUsdMonth()-want) > 0.05*want {
			t.Fatalf("spend %v, want ≈%v", payments.GetSpendUsdMonth(), want)
		}
		if _, err := cp.GetTeamSpend(ctx, connect.NewRequest(&kuberov1.GetTeamSpendRequest{Window: "bogus"})); err == nil {
			t.Fatal("bad window must be rejected")
		}
	})
}
