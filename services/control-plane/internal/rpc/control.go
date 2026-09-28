// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
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
	// Policies persists the CRD mirror + the armed kill-switch bit.
	// When nil, ArmPolicy returns a fixture-shaped success (unless
	// DemoFixturesDisabled, in which case it fails loudly).
	Policies store.PolicyStore
	// Alerts + AlertChannels page operators when a policy kill-switch
	// flips. When either is unset, ArmPolicy skips the notification —
	// the state change is still persisted + audited.
	Alerts        Alerter
	AlertChannels []string
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
	// Anomalies computes rolling z-scores over pod_cost_1s. When nil,
	// ListAnomalies falls back to the demo fixtures (unless
	// DemoFixturesDisabled).
	Anomalies *clickhouse.SpendAnomalyProvider
	// DemoFixturesDisabled hard-disables the demo-fixture fallback
	// (KUBEHERO_DEMO_MODE=false). RPCs that would otherwise serve
	// fixtures return FailedPrecondition instead, so a misconfigured
	// production deploy fails loudly rather than serving fake numbers.
	DemoFixturesDisabled bool
	// PG / CH are the raw store handles (nil when unconfigured) for
	// RPCs computed straight from the stores — rightsizing-backed waste,
	// team spend, capacity demands.
	PG *sql.DB
	CH *sql.DB
}

// Options is a tiny option-bag so callers can grow capability without
// changing the constructor signature again.
type Options struct {
	Clusters             store.ClusterStore
	Audit                store.AuditStore
	Policies             store.PolicyStore
	Alerts               Alerter
	AlertChannels        []string
	BurnRate             *clickhouse.BurnRateProvider
	PodCost              *clickhouse.PodCostWriter
	Anomalies            *clickhouse.SpendAnomalyProvider
	DemoFixturesDisabled bool
	PG                   *sql.DB
	CH                   *sql.DB
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
		if o.Policies != nil {
			cp.Policies = o.Policies
		}
		if o.Alerts != nil {
			cp.Alerts = o.Alerts
		}
		if len(o.AlertChannels) > 0 {
			cp.AlertChannels = o.AlertChannels
		}
		if o.BurnRate != nil {
			cp.BurnRate = o.BurnRate
		}
		if o.PodCost != nil {
			cp.PodCost = o.PodCost
		}
		if o.Anomalies != nil {
			cp.Anomalies = o.Anomalies
		}
		if o.DemoFixturesDisabled {
			cp.DemoFixturesDisabled = true
		}
		if o.PG != nil {
			cp.PG = o.PG
		}
		if o.CH != nil {
			cp.CH = o.CH
		}
	}
	return cp
}

// errDemoDisabled is what fixture-backed RPCs return when
// KUBEHERO_DEMO_MODE=false and no real store is wired to serve the
// call. FailedPrecondition (not Unavailable) because retrying won't
// help — the deployment needs DATABASE_URL / CLICKHOUSE_URL.
func errDemoDisabled(rpcName string) *connect.Error {
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%s: no backing store configured and demo fixtures are disabled (KUBEHERO_DEMO_MODE=false); set DATABASE_URL / CLICKHOUSE_URL", rpcName))
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
	var all []*kuberov1.Cluster
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
	if c.CH != nil {
		// Live node counts (distinct nodes reporting node_cost_1s in the
		// last 15m) replace registration-time counts, and clusters that
		// report without being registered are listed too. With
		// ClickHouse wired the list is live data only — never fixtures.
		all = c.liveClusters(ctx, all)
	} else if len(all) == 0 {
		if c.DemoFixturesDisabled {
			if c.Clusters == nil {
				return nil, errDemoDisabled("ListClusters")
			}
			// Store wired but empty: an honest empty list, not fixtures.
		} else {
			all = demoClusters()
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
		if c.DemoFixturesDisabled {
			if c.Audit == nil {
				return nil, errDemoDisabled("ListAuditLog")
			}
			// Store wired but empty: honest empty log, not fixtures.
		} else {
			entries = demoAuditEntries()
		}
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

// ListWasteRecommendations ranks workloads by what percentile-based
// rightsizing would recover ($/mo, 7-day window). Live when ClickHouse
// is wired; demo fixtures otherwise.
func (c *ControlPlane) ListWasteRecommendations(
	ctx context.Context,
	req *connect.Request[kuberov1.ListWasteRecommendationsRequest],
) (*connect.Response[kuberov1.ListWasteRecommendationsResponse], error) {
	if c.CH != nil {
		limit := int(req.Msg.GetLimit())
		if limit <= 0 {
			limit = 50
		}
		if limit > 500 {
			limit = 500
		}
		recs, err := c.liveWaste(ctx, strings.TrimSpace(req.Msg.GetClusterId()), limit)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("waste recommendations: %w", err))
		}
		return connect.NewResponse(&kuberov1.ListWasteRecommendationsResponse{Recommendations: recs}), nil
	}
	if c.DemoFixturesDisabled {
		return nil, errDemoDisabled("ListWasteRecommendations")
	}
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
	ctx context.Context,
	req *connect.Request[kuberov1.GetWorkloadRequest],
) (*connect.Response[kuberov1.GetWorkloadResponse], error) {
	cluster := strings.TrimSpace(req.Msg.GetCluster())
	namespace := strings.TrimSpace(req.Msg.GetNamespace())
	name := strings.TrimSpace(req.Msg.GetName())
	if cluster == "" || namespace == "" || name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("cluster, namespace and name are all required"))
	}
	if c.CH != nil {
		// Live: the workload's rightsizing rec (unset when nothing is
		// recoverable) + its audit history from Postgres, if wired.
		rec, history, err := c.liveWorkload(ctx, cluster, namespace, name)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get workload: %w", err))
		}
		return connect.NewResponse(&kuberov1.GetWorkloadResponse{Recommendation: rec, History: history}), nil
	}
	if c.DemoFixturesDisabled {
		return nil, errDemoDisabled("GetWorkload")
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

// ListPolicies lists the Budget/Ceiling policies mirrored into Postgres
// with their monthly ceiling and month-to-date spend % (ClickHouse).
func (c *ControlPlane) ListPolicies(
	ctx context.Context,
	req *connect.Request[kuberov1.ListPoliciesRequest],
) (*connect.Response[kuberov1.ListPoliciesResponse], error) {
	if lister, ok := c.Policies.(store.PolicyLister); ok {
		policies, err := c.livePolicies(ctx, lister, strings.TrimSpace(req.Msg.GetClusterId()), req.Msg.GetKind())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list policies: %w", err))
		}
		return connect.NewResponse(&kuberov1.ListPoliciesResponse{Policies: policies}), nil
	}
	if c.DemoFixturesDisabled {
		return nil, errDemoDisabled("ListPolicies")
	}
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
	if c.DemoFixturesDisabled {
		return nil, errDemoDisabled("ListVulnerabilities")
	}
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
//
// Live (ClickHouse): workloads with pods reported unschedulable in the
// last 30 minutes that haven't started since, priced with the cheapest
// node type seen in the cluster that fits one pod.
func (c *ControlPlane) ListCapacityDemands(
	ctx context.Context,
	req *connect.Request[kuberov1.ListCapacityDemandsRequest],
) (*connect.Response[kuberov1.ListCapacityDemandsResponse], error) {
	cluster := strings.TrimSpace(req.Msg.GetClusterId())
	var all []*kuberov1.CapacityDemand
	switch {
	case c.CH != nil:
		live, err := c.liveCapacity(ctx, cluster)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("capacity demands: %w", err))
		}
		all = live
	case c.DemoFixturesDisabled:
		return nil, errDemoDisabled("ListCapacityDemands")
	default:
		all = demoCapacityDemands()
		if cluster != "" {
			filtered := all[:0]
			for _, d := range all {
				if d.GetCluster() == cluster {
					filtered = append(filtered, d)
				}
			}
			all = filtered
		}
	}
	// Totals cover every demand, not just the returned page.
	var pods int32
	var blocked float64
	for _, d := range all {
		pods += d.GetPendingPods()
		blocked += d.GetBlockedCostUsdMonth()
	}
	limit := int(req.Msg.GetLimit())
	if limit > 0 && int32(len(all)) > int32(limit) {
		all = all[:limit]
	}
	return connect.NewResponse(&kuberov1.ListCapacityDemandsResponse{
		Demands:              all,
		TotalPendingPods:     pods,
		TotalBlockedUsdMonth: blocked,
	}), nil
}

// ListAnomalies returns the top-N statistically anomalous signals
// across the fleet, ranked by dollar impact. The dashboard's overview
// page surfaces these as one-line cards with a deep-link verb.
//
// When ClickHouse is wired the anomalies are real: a rolling z-score
// of each workload's last-hour spend against its trailing baseline
// (default 24h, request window "7d"/"30d" widens it), computed by
// clickhouse.SpendAnomalyProvider over pod_cost_1s and scored by the
// pure internal/anomaly package. |z| >= threshold (default 3.0,
// KUBEHERO_ANOMALY_Z_THRESHOLD) makes the list. Without ClickHouse we
// fall back to the curated demo set — same wire shape either way so
// the dashboard doesn't move. The request's `scope` field is accepted
// but ignored for now: detection is always workload-grained.
func (c *ControlPlane) ListAnomalies(
	ctx context.Context,
	req *connect.Request[kuberov1.ListAnomaliesRequest],
) (*connect.Response[kuberov1.ListAnomaliesResponse], error) {
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	var all []*kuberov1.Anomaly
	switch {
	case c.Anomalies != nil:
		window, err := parseAnomalyWindow(req.Msg.GetWindow())
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		hits, err := c.Anomalies.Detect(ctx, window)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("anomaly detect: %w", err))
		}
		threshold := c.Anomalies.EffectiveThreshold()
		all = make([]*kuberov1.Anomaly, 0, len(hits))
		for _, h := range hits {
			all = append(all, spendAnomalyToProto(h, threshold))
		}
		if c.CH != nil {
			// OOM-kill bursts ("capacity") and error-log spikes ("logs")
			// join the spend cards, ranked together by $ exposure.
			all = sortByImpact(append(all, c.signalAnomalies(ctx, window, threshold)...))
		}
	case c.DemoFixturesDisabled:
		return nil, errDemoDisabled("ListAnomalies")
	default:
		all = demoAnomalies()
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

// GetTeamSpend is chargeback per team / cost center. Live (ClickHouse):
// allocated compute + network spend over the window scaled to a 30-day
// month and split by cloud, recoverable $ from rightsizing, and idle
// GPU $ where GPU utilisation is reported.
func (c *ControlPlane) GetTeamSpend(
	ctx context.Context,
	req *connect.Request[kuberov1.GetTeamSpendRequest],
) (*connect.Response[kuberov1.GetTeamSpendResponse], error) {
	if err := requireFleet(ctx); err != nil {
		return nil, err
	}
	if c.CH != nil {
		resp, err := c.liveTeamSpend(ctx, req.Msg.GetWindow())
		if err != nil {
			if isInvalid(err) {
				return nil, connect.NewError(connect.CodeInvalidArgument, err)
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("team spend: %w", err))
		}
		return connect.NewResponse(resp), nil
	}
	if c.DemoFixturesDisabled {
		return nil, errDemoDisabled("GetTeamSpend")
	}
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
