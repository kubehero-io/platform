// SPDX-License-Identifier: BUSL-1.1
package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

func TestParseAnomalyWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"", 24 * time.Hour, false},
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{" 48H ", 48 * time.Hour, false},
		{"bogus", 0, true},
		{"1h", 0, true},   // below the 2h floor
		{"91d", 0, true},  // above the 90d ceiling
		{"-24h", 0, true}, // negative
		{"xd", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := parseAnomalyWindow(c.in)
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if err == nil && got != c.want {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}

func TestSpendAnomalyToProto(t *testing.T) {
	base := clickhouse.SpendAnomaly{
		ClusterID: "eks-use1-prod", Namespace: "retrieval", Workload: "vectordb-ingress",
		CurrentUSDHour: 40, MeanUSDHour: 10, StdDevUSDHour: 2,
		Z: 15, DeltaPct: 300, ImpactUSDMonth: (40 - 10) * 24 * 30,
	}

	t.Run("spike maps to critical above 2x threshold", func(t *testing.T) {
		p := spendAnomalyToProto(base, 3.0)
		if p.GetSeverity() != "critical" {
			t.Errorf("severity=%q want critical", p.GetSeverity())
		}
		if p.GetKind() != "spend" || p.GetSource() != "clickhouse" {
			t.Errorf("kind/source wrong: %q/%q", p.GetKind(), p.GetSource())
		}
		if p.GetSubject() != "retrieval/vectordb-ingress" {
			t.Errorf("subject=%q", p.GetSubject())
		}
		if p.GetLinkPath() != "/workloads/eks-use1-prod/retrieval/vectordb-ingress" {
			t.Errorf("link=%q", p.GetLinkPath())
		}
		if p.GetImpactUsdMonth() != 21600 {
			t.Errorf("impact=%v want 21600", p.GetImpactUsdMonth())
		}
		if p.GetDeltaPct() != 300 {
			t.Errorf("delta=%v want 300", p.GetDeltaPct())
		}
	})

	t.Run("moderate spike maps to warn", func(t *testing.T) {
		a := base
		a.Z = 4
		if got := spendAnomalyToProto(a, 3.0).GetSeverity(); got != "warn" {
			t.Errorf("severity=%q want warn", got)
		}
	})

	t.Run("drop maps to info with positive impact", func(t *testing.T) {
		a := base
		a.Z = -5
		a.DeltaPct = -80
		a.ImpactUSDMonth = -5760
		p := spendAnomalyToProto(a, 3.0)
		if p.GetSeverity() != "info" {
			t.Errorf("severity=%q want info", p.GetSeverity())
		}
		if p.GetImpactUsdMonth() != 5760 {
			t.Errorf("impact should be absolute for ranking, got %v", p.GetImpactUsdMonth())
		}
		if p.GetDeltaPct() != -80 {
			t.Errorf("delta must stay signed, got %v", p.GetDeltaPct())
		}
	})

	t.Run("id is stable per subject", func(t *testing.T) {
		a, b := base, base
		b.Z, b.CurrentUSDHour = 9, 99 // same workload, different reading
		if spendAnomalyToProto(a, 3).GetId() != spendAnomalyToProto(b, 3).GetId() {
			t.Error("id must be stable across refreshes of the same subject")
		}
		c := base
		c.Workload = "other"
		if spendAnomalyToProto(a, 3).GetId() == spendAnomalyToProto(c, 3).GetId() {
			t.Error("different subjects must get different ids")
		}
	})
}

func TestListAnomaliesDemoFallback(t *testing.T) {
	svc := New() // no ClickHouse wired
	res, err := svc.ListAnomalies(context.Background(),
		connect.NewRequest(&kuberov1.ListAnomaliesRequest{Limit: 2}))
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	if len(res.Msg.GetAnomalies()) != 2 {
		t.Fatalf("limit=2 returned %d", len(res.Msg.GetAnomalies()))
	}
	if res.Msg.GetTotal() != 4 {
		t.Fatalf("total=%d want 4 (full demo set)", res.Msg.GetTotal())
	}
	for _, a := range res.Msg.GetAnomalies() {
		if a.GetSource() != "demo" {
			t.Fatalf("stub mode must label anomalies as demo, got %q", a.GetSource())
		}
	}
}

// TestDemoFixturesDisabled locks in the KUBEHERO_DEMO_MODE=false
// contract: every RPC that would serve fixtures fails loudly with
// FailedPrecondition instead.
func TestDemoFixturesDisabled(t *testing.T) {
	svc := New(Options{DemoFixturesDisabled: true})
	ctx := context.Background()

	calls := []struct {
		name string
		call func() error
	}{
		{"ListClusters", func() error {
			_, err := svc.ListClusters(ctx, connect.NewRequest(&kuberov1.ListClustersRequest{}))
			return err
		}},
		{"ListAuditLog", func() error {
			_, err := svc.ListAuditLog(ctx, connect.NewRequest(&kuberov1.ListAuditLogRequest{}))
			return err
		}},
		{"ListWasteRecommendations", func() error {
			_, err := svc.ListWasteRecommendations(ctx, connect.NewRequest(&kuberov1.ListWasteRecommendationsRequest{}))
			return err
		}},
		{"GetWorkload", func() error {
			_, err := svc.GetWorkload(ctx, connect.NewRequest(&kuberov1.GetWorkloadRequest{
				Cluster: "c", Namespace: "n", Name: "w",
			}))
			return err
		}},
		{"ListPolicies", func() error {
			_, err := svc.ListPolicies(ctx, connect.NewRequest(&kuberov1.ListPoliciesRequest{}))
			return err
		}},
		{"ListVulnerabilities", func() error {
			_, err := svc.ListVulnerabilities(ctx, connect.NewRequest(&kuberov1.ListVulnerabilitiesRequest{}))
			return err
		}},
		{"ListCapacityDemands", func() error {
			_, err := svc.ListCapacityDemands(ctx, connect.NewRequest(&kuberov1.ListCapacityDemandsRequest{}))
			return err
		}},
		{"ListAnomalies", func() error {
			_, err := svc.ListAnomalies(ctx, connect.NewRequest(&kuberov1.ListAnomaliesRequest{}))
			return err
		}},
		{"GetTeamSpend", func() error {
			_, err := svc.GetTeamSpend(ctx, connect.NewRequest(&kuberov1.GetTeamSpendRequest{}))
			return err
		}},
	}
	for _, c := range calls {
		t.Run(c.name, func(t *testing.T) {
			err := c.call()
			if err == nil {
				t.Fatal("expected FailedPrecondition, got nil")
			}
			var ce *connect.Error
			if !errorsAs(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
				t.Fatalf("expected FailedPrecondition, got %v", err)
			}
		})
	}
}

// With fixtures disabled but real stores wired, list RPCs must return
// honest (possibly empty) data — never an error, never demo rows.
func TestDemoFixturesDisabledWithWiredStores(t *testing.T) {
	svc := New(Options{
		DemoFixturesDisabled: true,
		Clusters:             &fakeClusterStore{},
		Audit:                &fakeAuditStore{},
	})
	ctx := context.Background()

	cl, err := svc.ListClusters(ctx, connect.NewRequest(&kuberov1.ListClustersRequest{}))
	if err != nil {
		t.Fatalf("ListClusters with wired store: %v", err)
	}
	if n := len(cl.Msg.GetClusters()); n != 0 {
		t.Fatalf("empty store must yield empty list, got %d rows", n)
	}

	al, err := svc.ListAuditLog(ctx, connect.NewRequest(&kuberov1.ListAuditLogRequest{}))
	if err != nil {
		t.Fatalf("ListAuditLog with wired store: %v", err)
	}
	if n := len(al.Msg.GetEntries()); n != 0 {
		t.Fatalf("empty audit store must yield empty log, got %d entries", n)
	}
}
