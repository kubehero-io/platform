// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// Version is overridden at build time via -ldflags.
var Version = "0.0.0-dev"

// ControlPlane is the in-process implementation of the ControlPlaneService.
//
// All list-style RPCs share a fallback pattern: if no backing store is
// wired, the server returns demo data shaped like what the dashboard
// expects. That keeps `helm template` previews, the kind demo profile,
// and dashboard local dev working without a live Postgres + ClickHouse.
type ControlPlane struct {
	// Clusters persists cluster registrations.
	Clusters store.ClusterStore
	// Audit persists fired-policy events + structured operator actions.
	// When nil, ListAuditLog returns demo entries and AppendAuditEntry
	// is a no-op (the request still succeeds so callers don't error in
	// stub mode).
	Audit store.AuditStore
	// BurnRate reads pod_cost_1s in ClickHouse to compute the current
	// burn rate × 1000. When nil, GetBurnRate returns available=false
	// so the operator stays in "Tripped=Unknown" — never accidentally
	// trip without real data.
	BurnRate *clickhouse.BurnRateProvider
	// PodCost is the writer the IngestPodCost RPC funnels into. When
	// nil, the RPC accepts the request but reports written=0 + drops
	// silently; this keeps the kind-demo path working when the cp is
	// running without ClickHouse.
	PodCost *clickhouse.PodCostWriter
}

// Options is a tiny option-bag so callers can grow capability without
// changing the constructor signature again.
type Options struct {
	Clusters store.ClusterStore
	Audit    store.AuditStore
	BurnRate *clickhouse.BurnRateProvider
	PodCost  *clickhouse.PodCostWriter
}

func New(opts ...Options) *ControlPlane {
	cp := &ControlPlane{}
	for _, o := range opts {
		if o.Clusters != nil {
			cp.Clusters = o.Clusters
		}
		if o.Audit != nil {
			cp.Audit = o.Audit
		}
		if o.BurnRate != nil {
			cp.BurnRate = o.BurnRate
		}
		if o.PodCost != nil {
			cp.PodCost = o.PodCost
		}
	}
	return cp
}

// Compile-time assertion the server matches the generated interface.
var _ kuberov1connect.ControlPlaneServiceHandler = (*ControlPlane)(nil)

func (c *ControlPlane) HealthCheck(
	_ context.Context,
	_ *connect.Request[kuberov1.HealthCheckRequest],
) (*connect.Response[kuberov1.HealthCheckResponse], error) {
	return connect.NewResponse(&kuberov1.HealthCheckResponse{
		Status:  "ok",
		Version: Version,
	}), nil
}

func (c *ControlPlane) ListClusters(
	ctx context.Context,
	req *connect.Request[kuberov1.ListClustersRequest],
) (*connect.Response[kuberov1.ListClustersResponse], error) {
	all := demoClusters()
	if c.Clusters != nil {
		// best-effort overlay: if the store has rows, prefer them
		if rows, err := c.Clusters.List(ctx, "default", 100, 0); err == nil && len(rows) > 0 {
			all = make([]*kuberov1.Cluster, 0, len(rows))
			for _, r := range rows {
				all = append(all, &kuberov1.Cluster{
					Id: r.ID, Name: r.Name, Cloud: r.Cloud, Region: r.Region, Nodes: r.NodesCount,
				})
			}
		}
	}
	ps := req.Msg.GetPageSize()
	if ps > 0 && int32(len(all)) > ps {
		all = all[:ps]
	}
	return connect.NewResponse(&kuberov1.ListClustersResponse{
		Clusters: all,
	}), nil
}

var slugRE = regexp.MustCompile(`[^a-z0-9-]+`)

// RegisterCluster generates a UUID + a one-time enrollment token, stores
// only the SHA-256 hash, and returns the token plus a helm-install snippet
// pre-filled with the new cluster id.
func (c *ControlPlane) RegisterCluster(
	ctx context.Context,
	req *connect.Request[kuberov1.RegisterClusterRequest],
) (*connect.Response[kuberov1.RegisterClusterResponse], error) {
	if err := auth.Require(ctx, auth.RoleAdmin); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(req.Msg.GetName())
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("name is required"))
	}
	cloud := strings.ToLower(strings.TrimSpace(req.Msg.GetCloud()))
	switch cloud {
	case "aws", "gcp", "azure":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("cloud must be one of aws|gcp|azure, got %q", cloud))
	}
	region := strings.TrimSpace(req.Msg.GetRegion())
	if region == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("region is required"))
	}

	slug := strings.ToLower(strings.TrimSpace(req.Msg.GetSlug()))
	if slug == "" {
		slug = slugRE.ReplaceAllString(strings.ToLower(name), "-")
		slug = strings.Trim(slug, "-")
	}
	org := strings.TrimSpace(req.Msg.GetOrg())
	if org == "" {
		org = "default"
	}

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("token gen: %w", err))
	}
	token := hex.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	tokenHash := "sha256:" + hex.EncodeToString(hash[:])

	id := uuid.NewString()
	cluster := &store.Cluster{
		ID:              id,
		OrgID:           org,
		Slug:            slug,
		Name:            name,
		Cloud:           cloud,
		Region:          region,
		CertFingerprint: tokenHash, // re-uses the cert column until real mTLS lands
		State:           "healthy",
	}

	if c.Clusters != nil {
		if err := c.Clusters.Register(ctx, cluster); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("persist cluster: %w", err))
		}
	}

	helm := fmt.Sprintf(
		"helm install kubehero kubehero/kubehero \\\n"+
			"  --namespace kubehero-system --create-namespace \\\n"+
			"  --set cluster.id=%s \\\n"+
			"  --set cluster.token=%s",
		cluster.ID, token,
	)

	return connect.NewResponse(&kuberov1.RegisterClusterResponse{
		Cluster: &kuberov1.Cluster{
			Id: cluster.ID, Name: cluster.Name, Cloud: cluster.Cloud,
			Region: cluster.Region, Nodes: cluster.NodesCount,
		},
		Token:       token,
		HelmInstall: helm,
	}), nil
}

func (c *ControlPlane) ListAuditLog(
	ctx context.Context,
	req *connect.Request[kuberov1.ListAuditLogRequest],
) (*connect.Response[kuberov1.ListAuditLogResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 100
	}

	var entries []*kuberov1.AuditEntry
	if c.Audit != nil {
		rows, err := c.Audit.List(ctx, "default", limit*2) // overfetch so client filter still has room
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit list: %w", err))
		}
		for _, r := range rows {
			entries = append(entries, auditRowToProto(r))
		}
	}
	// Fallback to demo only when the store yielded nothing (stub mode or
	// freshly-installed control plane without any policy fires yet).
	if len(entries) == 0 {
		entries = demoAuditEntries()
	}

	if outcome := req.Msg.GetOutcome(); outcome != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.GetOutcome() == outcome {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if cluster := req.Msg.GetClusterId(); cluster != "" {
		filtered := entries[:0]
		for _, e := range entries {
			if e.GetCluster() == cluster {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	if int32(len(entries)) > int32(limit) {
		entries = entries[:limit]
	}
	return connect.NewResponse(&kuberov1.ListAuditLogResponse{Entries: entries}), nil
}

// GetBurnRate returns the current burn rate × 1000 over the given
// window. When ClickHouse is unwired or has no rows for the window, we
// return available=false; the operator maps that to Tripped=Unknown
// and refuses to trip without real data.
func (c *ControlPlane) GetBurnRate(
	ctx context.Context,
	req *connect.Request[kuberov1.GetBurnRateRequest],
) (*connect.Response[kuberov1.GetBurnRateResponse], error) {
	m := req.Msg
	if strings.TrimSpace(m.GetWindow()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("window is required"))
	}
	if c.BurnRate == nil || m.GetMonthlyCeilingUsd() <= 0 {
		return connect.NewResponse(&kuberov1.GetBurnRateResponse{
			BurnRateMilli: 0,
			Available:     false,
			Source:        "stub",
		}), nil
	}
	r, err := c.BurnRate.Compute(ctx, m.GetClusterId(), m.GetNamespace(), m.GetWindow(), m.GetMonthlyCeilingUsd())
	if err != nil {
		if errors.Is(err, clickhouse.ErrUnavailable) {
			return connect.NewResponse(&kuberov1.GetBurnRateResponse{
				BurnRateMilli: 0,
				Available:     false,
				Source:        "clickhouse",
			}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&kuberov1.GetBurnRateResponse{
		BurnRateMilli: r.BurnRateMilli,
		Available:     true,
		Source:        r.Source,
	}), nil
}

// AppendAuditEntry records a structured event. Server stamps the
// timestamp + HMAC signature; callers cannot forge either. In stub mode
// (no AuditStore wired) the call succeeds but persists nothing — that
// keeps the operator's reconcile loop simple in dev.
func (c *ControlPlane) AppendAuditEntry(
	ctx context.Context,
	req *connect.Request[kuberov1.AppendAuditEntryRequest],
) (*connect.Response[kuberov1.AppendAuditEntryResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	m := req.Msg
	if strings.TrimSpace(m.GetAction()) == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("action is required"))
	}
	now := time.Now().UTC()

	if c.Audit == nil {
		// Stub mode: pretend it succeeded so callers stay simple.
		return connect.NewResponse(&kuberov1.AppendAuditEntryResponse{
			Id: 0, At: now.Format(time.RFC3339Nano),
		}), nil
	}

	org := m.GetOrg()
	if org == "" {
		org = "default"
	}
	entry := &store.AuditEntry{
		At:         now,
		OrgID:      strPtrAudit(org),
		ClusterID:  strPtrAudit(m.GetClusterId()),
		ActorSub:   nonEmpty(m.GetActorSub(), "operator"),
		ActorEmail: m.GetActorEmail(),
		Action:     m.GetAction(),
		TargetKind: m.GetTargetKind(),
		TargetName: m.GetTargetName(),
		Payload:    m.GetPayload(),
		Outcome:    nonEmpty(m.GetOutcome(), "armed"),
	}
	id, err := c.Audit.Append(ctx, entry)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("audit append: %w", err))
	}
	return connect.NewResponse(&kuberov1.AppendAuditEntryResponse{
		Id: id,
		At: now.Format(time.RFC3339Nano),
	}), nil
}

// auditRowToProto flattens a store row into the proto wire shape. The
// dashboard renders cluster as a string column, so we surface either
// the persisted cluster slug (if known) or the fallback "—".
func auditRowToProto(r *store.AuditEntry) *kuberov1.AuditEntry {
	cluster := "—"
	if r.ClusterID != nil {
		cluster = *r.ClusterID
	}
	id := fmt.Sprintf("aud-%d", int64(r.At.UnixNano())%1_000_000)
	if r.RequestID != "" {
		id = r.RequestID
	}
	policy := r.TargetName
	if r.TargetKind != "" && r.TargetName != "" {
		policy = r.TargetKind + "/" + r.TargetName
	}
	// Pull effect_usd_month back out of the payload if the writer stamped it.
	var effect float64
	if len(r.Payload) > 0 {
		var p struct {
			EffectUsdMonth float64 `json:"effectUsdMonth"`
		}
		if err := json.Unmarshal(r.Payload, &p); err == nil {
			effect = p.EffectUsdMonth
		}
	}
	return &kuberov1.AuditEntry{
		Id:             id,
		At:             r.At.Format(time.RFC3339Nano),
		Policy:         policy,
		Action:         r.Action,
		Cluster:        cluster,
		Outcome:        r.Outcome,
		EffectUsdMonth: effect,
	}
}

func strPtrAudit(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonEmpty(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (c *ControlPlane) ListWasteRecommendations(
	_ context.Context,
	req *connect.Request[kuberov1.ListWasteRecommendationsRequest],
) (*connect.Response[kuberov1.ListWasteRecommendationsResponse], error) {
	recs := demoWaste()
	if lim := req.Msg.GetLimit(); lim > 0 && int32(len(recs)) > lim {
		recs = recs[:lim]
	}
	return connect.NewResponse(&kuberov1.ListWasteRecommendationsResponse{Recommendations: recs}), nil
}

// GetWorkload returns the current rightsize recommendation for a workload
// (matched on cluster + namespace + name) plus a small audit history
// scoped to that workload.
func (c *ControlPlane) GetWorkload(
	_ context.Context,
	req *connect.Request[kuberov1.GetWorkloadRequest],
) (*connect.Response[kuberov1.GetWorkloadResponse], error) {
	cluster := strings.TrimSpace(req.Msg.GetCluster())
	namespace := strings.TrimSpace(req.Msg.GetNamespace())
	name := strings.TrimSpace(req.Msg.GetName())
	if cluster == "" || namespace == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("cluster, namespace and name are all required"))
	}

	var rec *kuberov1.WasteRecommendation
	for _, r := range demoWaste() {
		if r.GetCluster() == cluster && r.GetNamespace() == namespace && r.GetWorkload() == name {
			rec = r
			break
		}
	}
	return connect.NewResponse(&kuberov1.GetWorkloadResponse{
		Recommendation: rec,
		History:        demoWorkloadHistory(cluster, namespace, name),
	}), nil
}

func (c *ControlPlane) ListPolicies(
	_ context.Context,
	req *connect.Request[kuberov1.ListPoliciesRequest],
) (*connect.Response[kuberov1.ListPoliciesResponse], error) {
	policies := demoPolicies()
	if kind := req.Msg.GetKind(); kind != "" {
		filtered := policies[:0]
		for _, p := range policies {
			if p.GetKind() == kind {
				filtered = append(filtered, p)
			}
		}
		policies = filtered
	}
	return connect.NewResponse(&kuberov1.ListPoliciesResponse{Policies: policies}), nil
}

func (c *ControlPlane) ListVulnerabilities(
	_ context.Context,
	req *connect.Request[kuberov1.ListVulnerabilitiesRequest],
) (*connect.Response[kuberov1.ListVulnerabilitiesResponse], error) {
	vulns := demoVulnerabilities()

	if cluster := req.Msg.GetClusterId(); cluster != "" {
		filtered := vulns[:0]
		for _, v := range vulns {
			if v.GetCluster() == cluster {
				filtered = append(filtered, v)
			}
		}
		vulns = filtered
	}
	if sev := strings.ToLower(req.Msg.GetSeverity()); sev != "" {
		filtered := vulns[:0]
		for _, v := range vulns {
			if v.GetSeverity() == sev {
				filtered = append(filtered, v)
			}
		}
		vulns = filtered
	}

	// Aggregate counts BEFORE applying limit so the dashboard header is honest.
	var crit, high, med, low int32
	for _, v := range vulns {
		switch v.GetSeverity() {
		case "critical":
			crit++
		case "high":
			high++
		case "medium":
			med++
		case "low":
			low++
		}
	}
	if lim := req.Msg.GetLimit(); lim > 0 && int32(len(vulns)) > lim {
		vulns = vulns[:lim]
	}
	return connect.NewResponse(&kuberov1.ListVulnerabilitiesResponse{
		Vulnerabilities: vulns,
		CriticalCount:   crit,
		HighCount:       high,
		MediumCount:     med,
		LowCount:        low,
	}), nil
}

// IngestPodCost is the high-throughput write path the collector calls
// every 5s with a batch of pod-second samples. Writes land in
// ClickHouse pod_cost_1s; everything downstream (burn-rate, sparklines,
// anomaly detection, /chargeback) reads from there.
//
// Auth: requires member-or-above role (collector tokens are typically
// admin so they pass; humans with member can also call this for
// debugging). cluster_id resolution: a slug like "eks-use1-prod"
// works; the writer's slug→UUID lookup happens at insert time so
// SCIM/operator-issued tokens carrying the cluster slug land cleanly.
//
// Idempotent on (cluster_id, pod, ts) — re-emitting a 5s window is
// harmless. Returns (written, dropped) so the caller can detect
// validation drops.
func (c *ControlPlane) IngestPodCost(
	ctx context.Context,
	req *connect.Request[kuberov1.IngestPodCostRequest],
) (*connect.Response[kuberov1.IngestPodCostResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	in := req.Msg.GetSamples()
	if len(in) == 0 {
		return connect.NewResponse(&kuberov1.IngestPodCostResponse{}), nil
	}
	if c.PodCost == nil {
		// Stub mode: silently accept the batch so the kind-demo +
		// unit-test paths don't fail when ClickHouse isn't wired.
		return connect.NewResponse(&kuberov1.IngestPodCostResponse{
			Written: 0,
			Dropped: int32(len(in)),
		}), nil
	}

	// Default cluster_id from request top-level if individual samples
	// don't carry it. The collector typically sets it once on the
	// request and leaves it off the per-sample shape.
	defaultCluster := strings.TrimSpace(req.Msg.GetClusterId())

	batch := make([]clickhouse.Sample, 0, len(in))
	for _, s := range in {
		cluster := s.GetCluster()
		if cluster == "" {
			cluster = defaultCluster
		}
		batch = append(batch, clickhouse.Sample{
			OrgID:      "default", // resolved per-cluster once orgs are wired
			ClusterID:  cluster,
			Node:       s.GetNode(),
			Namespace:  s.GetNamespace(),
			Pod:        s.GetPod(),
			Team:       s.GetTeam(),
			CostCenter: s.GetCostCenter(),
			Nodepool:   s.GetNodepool(),
			Region:     s.GetRegion(),
			SKU:        s.GetSku(),
			Lifecycle:  s.GetLifecycle(),
			GPUKind:    s.GetGpuKind(),
			CPUMilli:   s.GetCpuMillicores(),
			MemBytes:   s.GetMemBytes(),
			GPUUtilPct: s.GetGpuUtilPct(),
			CostUSDSec: s.GetCostUsdSec(),
			RecoverUSD: s.GetRecoverableUsdSec(),
			TsUnixMS:   s.GetTsUnixMs(),
		})
	}

	written, dropped, err := c.PodCost.Insert(ctx, batch)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("clickhouse write: %w", err))
	}
	return connect.NewResponse(&kuberov1.IngestPodCostResponse{
		Written: int32(written),
		Dropped: int32(dropped),
	}), nil
}

// ListCapacityDemands surfaces unschedulable pods + recommended
// capacity additions to unblock them, ranked by $/mo of blocked work.
// Borrowed pattern from Ray's autoscaler dashboard but bound to
// dollars rather than abstract resources, so the operator can decide
// whether a $1.8k/mo capacity bump unblocks $6.1k/mo of blocked work
// (yes) or $200/mo of blocked work (probably no).
func (c *ControlPlane) ListCapacityDemands(
	_ context.Context,
	req *connect.Request[kuberov1.ListCapacityDemandsRequest],
) (*connect.Response[kuberov1.ListCapacityDemandsResponse], error) {
	all := demoCapacityDemands()
	if cluster := req.Msg.GetClusterId(); cluster != "" {
		filtered := all[:0]
		for _, d := range all {
			if d.GetCluster() == cluster {
				filtered = append(filtered, d)
			}
		}
		all = filtered
	}
	limit := int(req.Msg.GetLimit())
	if limit > 0 && int32(len(all)) > int32(limit) {
		all = all[:limit]
	}
	var pods int32
	var blocked float64
	for _, d := range all {
		pods += d.GetPendingPods()
		blocked += d.GetBlockedCostUsdMonth()
	}
	return connect.NewResponse(&kuberov1.ListCapacityDemandsResponse{
		Demands:               all,
		TotalPendingPods:      pods,
		TotalBlockedUsdMonth:  blocked,
	}), nil
}

// ListAnomalies returns the top-N statistically anomalous signals
// across the fleet, ranked by dollar impact. The dashboard's overview
// page surfaces these as one-line cards with a deep-link verb.
//
// Today the ranking is deterministic over a curated demo set; once
// the ingest pipeline lands the implementation will be a rolling
// z-score over pod_cost_1s + a join against the audit + vulnerability
// rows. The wire shape stays unchanged so the dashboard doesn't move.
func (c *ControlPlane) ListAnomalies(
	_ context.Context,
	req *connect.Request[kuberov1.ListAnomaliesRequest],
) (*connect.Response[kuberov1.ListAnomaliesResponse], error) {
	all := demoAnomalies()
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	out := all
	if int32(len(out)) > int32(limit) {
		out = out[:limit]
	}
	return connect.NewResponse(&kuberov1.ListAnomaliesResponse{
		Anomalies: out,
		Total:     int32(len(all)),
	}), nil
}

func (c *ControlPlane) GetTeamSpend(
	_ context.Context,
	_ *connect.Request[kuberov1.GetTeamSpendRequest],
) (*connect.Response[kuberov1.GetTeamSpendResponse], error) {
	teams := demoTeamSpend()
	var total, recoverable float64
	for _, t := range teams {
		total += t.GetSpendUsdMonth()
		recoverable += t.GetRecoverableUsdMonth()
	}
	return connect.NewResponse(&kuberov1.GetTeamSpendResponse{
		Teams:                    teams,
		FleetTotalUsdMonth:       total,
		FleetRecoverableUsdMonth: recoverable,
	}), nil
}
