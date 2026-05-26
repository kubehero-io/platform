// SPDX-License-Identifier: BUSL-1.1
package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// adminCtx returns a context with an admin principal so tests don't
// have to set up the Connect interceptor stack just to call protected
// handlers directly.
func adminCtx() context.Context {
	return auth.WithPrincipal(context.Background(),
		auth.Principal{Sub: "test-admin", Role: auth.RoleAdmin})
}

// fakeClusterStore is the minimum surface we need for RegisterCluster tests.
type fakeClusterStore struct {
	mu       sync.Mutex
	clusters []*store.Cluster
}

func (f *fakeClusterStore) Register(_ context.Context, c *store.Cluster) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clusters = append(f.clusters, c)
	return nil
}
func (f *fakeClusterStore) Get(_ context.Context, id string) (*store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.clusters {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, nil
}
func (f *fakeClusterStore) List(_ context.Context, _ string, _, _ int) ([]*store.Cluster, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*store.Cluster(nil), f.clusters...), nil
}
func (f *fakeClusterStore) Touch(_ context.Context, _ string, _ int32, _ string) error { return nil }

// fakeAuditStore mirrors the persisted entries in memory so tests can
// assert what the server wrote.
type fakeAuditStore struct {
	mu      sync.Mutex
	entries []*store.AuditEntry
}

func (f *fakeAuditStore) Append(_ context.Context, e *store.AuditEntry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return int64(len(f.entries)), nil
}
func (f *fakeAuditStore) List(_ context.Context, _ string, limit int) ([]*store.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]*store.AuditEntry(nil), f.entries...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func TestHealthCheck(t *testing.T) {
	r, err := New().HealthCheck(context.Background(),
		connect.NewRequest(&kuberov1.HealthCheckRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if r.Msg.GetStatus() != "ok" {
		t.Fatalf("status=%q want ok", r.Msg.GetStatus())
	}
}

func TestListClustersPagination(t *testing.T) {
	svc := New()
	full, _ := svc.ListClusters(context.Background(),
		connect.NewRequest(&kuberov1.ListClustersRequest{}))
	if len(full.Msg.GetClusters()) != 6 {
		t.Fatalf("got %d clusters, want 6", len(full.Msg.GetClusters()))
	}
	pg, _ := svc.ListClusters(context.Background(),
		connect.NewRequest(&kuberov1.ListClustersRequest{PageSize: 3}))
	if len(pg.Msg.GetClusters()) != 3 {
		t.Fatalf("paged got %d, want 3", len(pg.Msg.GetClusters()))
	}
}

func TestListAuditLogFilters(t *testing.T) {
	svc := New()
	all, _ := svc.ListAuditLog(context.Background(),
		connect.NewRequest(&kuberov1.ListAuditLogRequest{}))
	if len(all.Msg.GetEntries()) == 0 {
		t.Fatal("expected demo audit entries")
	}

	applied, _ := svc.ListAuditLog(context.Background(),
		connect.NewRequest(&kuberov1.ListAuditLogRequest{Outcome: "applied"}))
	for _, e := range applied.Msg.GetEntries() {
		if e.GetOutcome() != "applied" {
			t.Fatalf("outcome filter leaked %q", e.GetOutcome())
		}
	}

	limited, _ := svc.ListAuditLog(context.Background(),
		connect.NewRequest(&kuberov1.ListAuditLogRequest{Limit: 2}))
	if len(limited.Msg.GetEntries()) != 2 {
		t.Fatalf("limit=2 returned %d", len(limited.Msg.GetEntries()))
	}

	cluster, _ := svc.ListAuditLog(context.Background(),
		connect.NewRequest(&kuberov1.ListAuditLogRequest{ClusterId: "eks-use1-prod"}))
	for _, e := range cluster.Msg.GetEntries() {
		if e.GetCluster() != "eks-use1-prod" {
			t.Fatalf("cluster filter leaked %q", e.GetCluster())
		}
	}
}

func TestListWasteRecommendationsLimit(t *testing.T) {
	svc := New()
	r, _ := svc.ListWasteRecommendations(context.Background(),
		connect.NewRequest(&kuberov1.ListWasteRecommendationsRequest{Limit: 3}))
	if len(r.Msg.GetRecommendations()) != 3 {
		t.Fatalf("got %d recs, want 3", len(r.Msg.GetRecommendations()))
	}
	if r.Msg.GetRecommendations()[0].GetRecoverableUsdMonth() <= 0 {
		t.Fatal("expected positive recoverable amount")
	}
}

func TestListPoliciesKindFilter(t *testing.T) {
	svc := New()
	all, _ := svc.ListPolicies(context.Background(),
		connect.NewRequest(&kuberov1.ListPoliciesRequest{}))
	if len(all.Msg.GetPolicies()) == 0 {
		t.Fatal("expected demo policies")
	}
	ceilings, _ := svc.ListPolicies(context.Background(),
		connect.NewRequest(&kuberov1.ListPoliciesRequest{Kind: "CeilingPolicy"}))
	for _, p := range ceilings.Msg.GetPolicies() {
		if p.GetKind() != "CeilingPolicy" {
			t.Fatalf("kind filter leaked %q", p.GetKind())
		}
	}
}

func TestRegisterClusterPersistsHashedToken(t *testing.T) {
	fake := &fakeClusterStore{}
	svc := New(Options{Clusters: fake})
	res, err := svc.RegisterCluster(adminCtx(), connect.NewRequest(&kuberov1.RegisterClusterRequest{
		Name: "Prod EU 1", Cloud: "AWS", Region: "eu-west-1",
	}))
	if err != nil {
		t.Fatalf("RegisterCluster: %v", err)
	}
	if res.Msg.GetToken() == "" || len(res.Msg.GetToken()) != 64 {
		t.Fatalf("expected 64-hex-char token, got %d chars", len(res.Msg.GetToken()))
	}
	if res.Msg.GetCluster().GetId() == "" {
		t.Fatal("cluster id missing")
	}
	if !strings.Contains(res.Msg.GetHelmInstall(), res.Msg.GetCluster().GetId()) {
		t.Fatal("helm snippet missing cluster id")
	}
	if !strings.Contains(res.Msg.GetHelmInstall(), res.Msg.GetToken()) {
		t.Fatal("helm snippet missing token")
	}

	if len(fake.clusters) != 1 {
		t.Fatalf("expected 1 persisted cluster, got %d", len(fake.clusters))
	}
	persisted := fake.clusters[0]
	if persisted.Cloud != "aws" {
		t.Fatalf("cloud not lowercased on store: %q", persisted.Cloud)
	}
	if persisted.Slug != "prod-eu-1" {
		t.Fatalf("slug not normalised: %q", persisted.Slug)
	}
	want := "sha256:" + hex.EncodeToString(sha256Of(res.Msg.GetToken()))
	if persisted.CertFingerprint != want {
		t.Fatalf("hash mismatch: %q vs %q", persisted.CertFingerprint, want)
	}
}

func sha256Of(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func TestRegisterClusterRejectsInvalidArgs(t *testing.T) {
	svc := New()
	cases := []struct {
		name string
		req  *kuberov1.RegisterClusterRequest
	}{
		{"empty name", &kuberov1.RegisterClusterRequest{Cloud: "aws", Region: "us-east-1"}},
		{"bad cloud", &kuberov1.RegisterClusterRequest{Name: "x", Cloud: "linode", Region: "us-east-1"}},
		{"empty region", &kuberov1.RegisterClusterRequest{Name: "x", Cloud: "aws"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.RegisterCluster(adminCtx(), connect.NewRequest(c.req))
			if err == nil {
				t.Fatal("expected error")
			}
			var connectErr *connect.Error
			if !errorsAs(err, &connectErr) || connectErr.Code() != connect.CodeInvalidArgument {
				t.Fatalf("expected InvalidArgument, got %v", err)
			}
		})
	}
}

// errorsAs is a tiny wrapper over errors.As so the imports above stay tight.
func errorsAs(err error, target **connect.Error) bool {
	for err != nil {
		if ce, ok := err.(*connect.Error); ok {
			*target = ce
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestAppendAuditEntryPersistsAndIsListable(t *testing.T) {
	fake := &fakeAuditStore{}
	svc := New(Options{Audit: fake})
	res, err := svc.AppendAuditEntry(adminCtx(), connect.NewRequest(&kuberov1.AppendAuditEntryRequest{
		Action:     "ceiling.tripped",
		TargetKind: "CeilingPolicy",
		TargetName: "prod-burn-rate-2x",
		ClusterId:  "eks-use1-prod",
		Outcome:    "armed",
		Payload:    []byte(`{"effectUsdMonth":8600}`),
	}))
	if err != nil {
		t.Fatalf("AppendAuditEntry: %v", err)
	}
	if res.Msg.GetId() != 1 {
		t.Fatalf("expected id=1, got %d", res.Msg.GetId())
	}
	if len(fake.entries) != 1 {
		t.Fatalf("expected 1 persisted entry, got %d", len(fake.entries))
	}
	persisted := fake.entries[0]
	if persisted.Action != "ceiling.tripped" {
		t.Errorf("action lost: %q", persisted.Action)
	}
	if persisted.ActorSub != "operator" {
		t.Errorf("actor_sub default not applied: %q", persisted.ActorSub)
	}
	if persisted.At.IsZero() {
		t.Error("server should stamp timestamp")
	}

	// Round-trip via ListAuditLog
	lst, err := svc.ListAuditLog(context.Background(), connect.NewRequest(&kuberov1.ListAuditLogRequest{}))
	if err != nil {
		t.Fatalf("ListAuditLog: %v", err)
	}
	if len(lst.Msg.GetEntries()) != 1 {
		t.Fatalf("expected 1 entry from store, got %d", len(lst.Msg.GetEntries()))
	}
	got := lst.Msg.GetEntries()[0]
	if got.GetPolicy() != "CeilingPolicy/prod-burn-rate-2x" {
		t.Errorf("policy formatting wrong: %q", got.GetPolicy())
	}
	if got.GetEffectUsdMonth() != 8600 {
		t.Errorf("effect not extracted from payload: %v", got.GetEffectUsdMonth())
	}
}

func TestAppendAuditEntryRequiresAction(t *testing.T) {
	svc := New(Options{Audit: &fakeAuditStore{}})
	_, err := svc.AppendAuditEntry(adminCtx(), connect.NewRequest(&kuberov1.AppendAuditEntryRequest{}))
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestGetBurnRateStubModeReturnsUnavailable(t *testing.T) {
	svc := New() // no BurnRate provider
	res, err := svc.GetBurnRate(context.Background(), connect.NewRequest(&kuberov1.GetBurnRateRequest{
		Namespace: "ml-inference", Window: "5m", MonthlyCeilingUsd: 10000,
	}))
	if err != nil {
		t.Fatalf("GetBurnRate: %v", err)
	}
	if res.Msg.GetAvailable() {
		t.Error("stub mode should return available=false")
	}
	if res.Msg.GetSource() != "stub" {
		t.Errorf("source=%q want stub", res.Msg.GetSource())
	}
}

func TestGetBurnRateRequiresWindow(t *testing.T) {
	svc := New()
	_, err := svc.GetBurnRate(context.Background(), connect.NewRequest(&kuberov1.GetBurnRateRequest{
		Namespace: "ml-inference",
	}))
	if err == nil {
		t.Fatal("expected InvalidArgument")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestAppendAuditEntryStubModeIsNoOp(t *testing.T) {
	svc := New() // no AuditStore
	res, err := svc.AppendAuditEntry(adminCtx(), connect.NewRequest(&kuberov1.AppendAuditEntryRequest{
		Action: "ceiling.tripped",
	}))
	if err != nil {
		t.Fatalf("AppendAuditEntry: %v", err)
	}
	if res.Msg.GetId() != 0 {
		t.Errorf("stub-mode should return id=0, got %d", res.Msg.GetId())
	}
	if res.Msg.GetAt() == "" {
		t.Error("stub-mode should still stamp At")
	}
}

func TestIngestPodCostStubModeIsNoOp(t *testing.T) {
	svc := New() // no PodCost writer
	res, err := svc.IngestPodCost(adminCtx(), connect.NewRequest(&kuberov1.IngestPodCostRequest{
		ClusterId: "eks-use1-prod",
		Samples: []*kuberov1.PodCostSample{
			{Pod: "p1", Cluster: "eks-use1-prod", CostUsdSec: 0.001},
		},
	}))
	if err != nil {
		t.Fatalf("IngestPodCost: %v", err)
	}
	// Stub mode reports the batch as dropped (not written) so the
	// caller can metric the silent-accept path.
	if res.Msg.GetDropped() != 1 || res.Msg.GetWritten() != 0 {
		t.Fatalf("stub mode should drop the batch silently: %+v", res.Msg)
	}
}

func TestIngestPodCostRequiresMember(t *testing.T) {
	svc := New()
	// Anonymous (no Principal stamped) — Require(member) should reject.
	_, err := svc.IngestPodCost(context.Background(), connect.NewRequest(&kuberov1.IngestPodCostRequest{
		Samples: []*kuberov1.PodCostSample{{Pod: "p1", Cluster: "x"}},
	}))
	if err == nil {
		t.Fatal("expected PermissionDenied for unauthenticated caller")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodePermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", err)
	}
}

func TestGetWorkloadReturnsRecAndHistory(t *testing.T) {
	svc := New()
	res, err := svc.GetWorkload(context.Background(), connect.NewRequest(&kuberov1.GetWorkloadRequest{
		Cluster: "eks-use1-prod", Namespace: "retrieval", Name: "vectordb-ingress",
	}))
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if res.Msg.GetRecommendation() == nil {
		t.Fatal("expected matching demo recommendation")
	}
	if got := res.Msg.GetRecommendation().GetWorkload(); got != "vectordb-ingress" {
		t.Fatalf("workload mismatch: %q", got)
	}
	if len(res.Msg.GetHistory()) == 0 {
		t.Fatal("expected at least one history entry")
	}
}

func TestGetWorkloadUnknownReturnsHistoryWithoutRec(t *testing.T) {
	svc := New()
	res, err := svc.GetWorkload(context.Background(), connect.NewRequest(&kuberov1.GetWorkloadRequest{
		Cluster: "eks-use1-prod", Namespace: "retrieval", Name: "does-not-exist",
	}))
	if err != nil {
		t.Fatalf("GetWorkload: %v", err)
	}
	if res.Msg.GetRecommendation() != nil {
		t.Fatal("did not expect a rec for unknown workload")
	}
	// History is synthetic per-name so it should still be present.
	if len(res.Msg.GetHistory()) == 0 {
		t.Fatal("expected synthetic history for unknown workload")
	}
}

func TestGetWorkloadRequiresAllParts(t *testing.T) {
	svc := New()
	_, err := svc.GetWorkload(context.Background(), connect.NewRequest(&kuberov1.GetWorkloadRequest{
		Cluster: "x", Namespace: "y",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	var ce *connect.Error
	if !errorsAs(err, &ce) || ce.Code() != connect.CodeInvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
}

func TestListVulnerabilitiesAggregatesAndFilters(t *testing.T) {
	svc := New()

	all, _ := svc.ListVulnerabilities(context.Background(),
		connect.NewRequest(&kuberov1.ListVulnerabilitiesRequest{}))
	if all.Msg.GetCriticalCount()+all.Msg.GetHighCount()+all.Msg.GetMediumCount()+all.Msg.GetLowCount() == 0 {
		t.Fatal("expected aggregate counts > 0")
	}
	if len(all.Msg.GetVulnerabilities()) == 0 {
		t.Fatal("expected demo vulnerabilities")
	}

	crit, _ := svc.ListVulnerabilities(context.Background(),
		connect.NewRequest(&kuberov1.ListVulnerabilitiesRequest{Severity: "critical"}))
	for _, v := range crit.Msg.GetVulnerabilities() {
		if v.GetSeverity() != "critical" {
			t.Fatalf("severity filter leaked %q", v.GetSeverity())
		}
	}

	cluster, _ := svc.ListVulnerabilities(context.Background(),
		connect.NewRequest(&kuberov1.ListVulnerabilitiesRequest{ClusterId: "eks-use1-prod"}))
	for _, v := range cluster.Msg.GetVulnerabilities() {
		if v.GetCluster() != "eks-use1-prod" {
			t.Fatalf("cluster filter leaked %q", v.GetCluster())
		}
	}

	limited, _ := svc.ListVulnerabilities(context.Background(),
		connect.NewRequest(&kuberov1.ListVulnerabilitiesRequest{Limit: 2}))
	if len(limited.Msg.GetVulnerabilities()) != 2 {
		t.Fatalf("limit=2 returned %d", len(limited.Msg.GetVulnerabilities()))
	}
	// Counts reflect pre-limit total, not the trimmed page.
	if limited.Msg.GetCriticalCount() == 0 {
		t.Fatal("aggregate counts should reflect full set even when limited")
	}
}

func TestGetTeamSpendTotals(t *testing.T) {
	svc := New()
	r, _ := svc.GetTeamSpend(context.Background(),
		connect.NewRequest(&kuberov1.GetTeamSpendRequest{Window: "30d"}))
	if len(r.Msg.GetTeams()) == 0 {
		t.Fatal("expected demo team data")
	}
	var sum float64
	for _, t := range r.Msg.GetTeams() {
		sum += t.GetSpendUsdMonth()
	}
	if got := r.Msg.GetFleetTotalUsdMonth(); got != sum {
		t.Fatalf("fleet total %.0f != sum %.0f", got, sum)
	}
}
