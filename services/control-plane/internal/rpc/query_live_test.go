// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/budget"
	"github.com/kubehero-io/platform/services/control-plane/internal/rightsizing"
)

func TestWasteFromRecsAggregatesPerWorkload(t *testing.T) {
	recs := []rightsizing.Recommendation{
		{Cluster: "c1", Namespace: "shop", Workload: "api", Container: "app", Direction: rightsizing.Downsize,
			SavingsMonth: 300, CPUSavingsMonth: 280, MemSavingsMonth: 20, CPURequest: 2, CPUP95: 0.31, Confidence: "high", Cloud: "aws"},
		{Cluster: "c1", Namespace: "shop", Workload: "api", Container: "sidecar", Direction: rightsizing.Downsize,
			SavingsMonth: 20, Confidence: "high", Cloud: "aws"},
		{Cluster: "c1", Namespace: "shop", Workload: "cache", Container: "redis", Direction: rightsizing.Downsize,
			SavingsMonth: 500, CPUSavingsMonth: 10, MemSavingsMonth: 490, MemRequest: 8 << 30, MemMax: 1.5 * (1 << 30),
			Confidence: "medium", Cloud: "gcp", ThrottleRisk: 0.2},
		{Cluster: "c1", Namespace: "shop", Workload: "db", Direction: rightsizing.Upsize, SavingsMonth: -50},
		{Cluster: "c1", Namespace: "shop", Workload: "idle", Direction: rightsizing.OK},
	}
	agg := wasteFromRecs(recs)
	if len(agg) != 2 || agg[0].workload != "cache" || agg[1].workload != "api" {
		t.Fatalf("aggregation: %+v", agg)
	}
	if agg[1].savings != 320 {
		t.Fatalf("api savings = %v", agg[1].savings)
	}
	api := agg[1].toProto(2)
	if api.GetRank() != "02" || api.GetCloud() != "EKS" || api.GetSignal() != "cpu.req=2.0 p95=0.31" ||
		api.GetAction() != "apply" || api.GetSeverity() != "accent" || api.GetRecoverableUsdMonth() != 320 {
		t.Fatalf("api proto: %+v", api)
	}
	cache := agg[0].toProto(1)
	if cache.GetSignal() != "mem.req=8Gi max=1.5Gi" || cache.GetAction() != "review" || cache.GetSeverity() != "warn" || cache.GetCloud() != "GKE" {
		t.Fatalf("cache proto: %+v", cache)
	}
}

func TestFormattingHelpers(t *testing.T) {
	for in, want := range map[float64]string{2: "2.0", 0.31: "0.31", 16: "16.0", 0.312: "0.31", 1.5: "1.5"} {
		if got := oneDecimal(in); got != want {
			t.Errorf("oneDecimal(%v) = %q, want %q", in, got, want)
		}
	}
	if k8sBytes(412<<20) != "412Mi" || k8sBytes(1<<30) != "1Gi" {
		t.Error("k8sBytes")
	}
	if managedName("azure") != "AKS" || managedName("onprem") != "" {
		t.Error("managedName")
	}
}

func TestScopedSpend(t *testing.T) {
	mtd := map[[2]string]float64{
		{"uuid-1", "ml"}: 100, {"prod", "ml"}: 50, {"uuid-1", "web"}: 30, {"other", "ml"}: 1000,
	}
	all, _ := budget.ParseSpec([]byte(`{}`))
	if got := scopedSpend(mtd, []string{"uuid-1", "prod"}, all); got != 180 {
		t.Fatalf("cluster-wide = %v", got)
	}
	ml, _ := budget.ParseSpec([]byte(`{"scope":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"ml"}}}}`))
	if got := scopedSpend(mtd, []string{"uuid-1", "prod"}, ml); got != 150 {
		t.Fatalf("namespace scope = %v", got)
	}
}

func TestSortByImpact(t *testing.T) {
	out := sortByImpact([]*kuberov1.Anomaly{{Id: "a", ImpactUsdMonth: 1}, {Id: "b", ImpactUsdMonth: 5}, {Id: "c", ImpactUsdMonth: 3}})
	if out[0].GetId() != "b" || out[2].GetId() != "a" {
		t.Fatalf("order %v %v %v", out[0].GetId(), out[1].GetId(), out[2].GetId())
	}
}

func TestTeamSpendDeniesClusterTokens(t *testing.T) {
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Role: auth.RoleMember, ClusterID: "c1"})
	_, err := New().GetTeamSpend(ctx, connect.NewRequest(&kuberov1.GetTeamSpendRequest{}))
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("cluster token on fleet chargeback: %v", err)
	}
}
