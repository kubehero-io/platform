// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rpc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/budget"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/insights"
	"github.com/kubehero-io/platform/services/control-plane/internal/rightsizing"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// liveDeps are the query engines behind the store-backed ControlPlane
// RPCs. They're built lazily per (ClickHouse, Postgres) handle pair so
// ControlPlane stays a plain struct and the rightsizing cache and
// cluster-name cache are shared across calls.
type liveDeps struct {
	clusters    *clusters.Resolver
	cost        *cost.Engine
	rightsizing *rightsizing.Engine
	insights    *insights.Engine
}

var (
	liveMu   sync.Mutex
	liveByDB = map[[2]*sql.DB]*liveDeps{}
)

func (c *ControlPlane) live() *liveDeps {
	key := [2]*sql.DB{c.CH, c.PG}
	liveMu.Lock()
	defer liveMu.Unlock()
	if d, ok := liveByDB[key]; ok {
		return d
	}
	res := clusters.NewResolver(c.PG, slog.Default())
	d := &liveDeps{
		clusters:    res,
		cost:        &cost.Engine{CH: c.CH, Clusters: res},
		rightsizing: &rightsizing.Engine{CH: c.CH, Clusters: res},
		insights:    &insights.Engine{CH: c.CH, Clusters: res},
	}
	liveByDB[key] = d
	return d
}

// ─── ListClusters ────────────────────────────────────────────────────

// liveClusterNodes overlays node counts measured in the last 15
// minutes (distinct nodes reporting node_cost_1s) onto the registered
// clusters, and appends clusters that report data without being
// registered.
func (c *ControlPlane) liveClusterNodes(ctx context.Context, registered []*kuberov1.Cluster) ([]*kuberov1.Cluster, error) {
	snap := c.live().clusters.Snapshot(ctx)
	qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	rows, err := c.CH.QueryContext(qctx, `
		SELECT cluster_id, uniqExact(node), any(cloud), any(region)
		FROM node_cost_1s WHERE ts >= ?
		GROUP BY cluster_id LIMIT 5000`, time.Now().Add(-15*time.Minute).UnixMilli())
	if err != nil {
		return registered, fmt.Errorf("cluster nodes query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	byID := map[string]*kuberov1.Cluster{}
	for _, cl := range registered {
		byID[cl.GetId()] = cl
	}
	out := registered
	measured := map[*kuberov1.Cluster]int32{}
	for rows.Next() {
		var (
			id, cloud, region string
			nodes             uint64
		)
		if err := rows.Scan(&id, &nodes, &cloud, &region); err != nil {
			return registered, fmt.Errorf("cluster nodes scan: %w", err)
		}
		target := byID[id]
		if info, ok := snap.Lookup(id); ok && target == nil {
			target = byID[info.ID]
		}
		if target == nil {
			// Reporting but never registered (static API key collector).
			target = &kuberov1.Cluster{Id: id, Name: snap.Display(id), Cloud: cloud, Region: region}
			byID[id] = target
			out = append(out, target)
		}
		// A cluster may report under both its UUID and its slug.
		measured[target] += int32(nodes)
	}
	if err := rows.Err(); err != nil {
		return registered, err
	}
	for cl, n := range measured {
		cl.Nodes = n // measured beats the registration-time count
	}
	return out, nil
}

// liveClusters degrades to the registered list when ClickHouse fails.
func (c *ControlPlane) liveClusters(ctx context.Context, registered []*kuberov1.Cluster) []*kuberov1.Cluster {
	out, err := c.liveClusterNodes(ctx, registered)
	if err != nil {
		slog.Default().Warn("list clusters: live node counts unavailable", "err", err)
	}
	return out
}

// ─── Waste / GetWorkload ─────────────────────────────────────────────

const wasteWindow = "7d"

type workloadWaste struct {
	cluster, namespace, workload, cloud string
	savings                             float64
	top                                 rightsizing.Recommendation
	lowConfidence                       bool
	risky                               bool
}

// wasteFromRecs folds container recommendations into one entry per
// workload (downsizes with positive savings only), largest first.
func wasteFromRecs(recs []rightsizing.Recommendation) []*workloadWaste {
	by := map[[3]string]*workloadWaste{}
	for _, r := range recs {
		if r.Direction != rightsizing.Downsize || r.SavingsMonth <= 0 {
			continue
		}
		k := [3]string{r.Cluster, r.Namespace, r.Workload}
		w := by[k]
		if w == nil {
			w = &workloadWaste{cluster: r.Cluster, namespace: r.Namespace, workload: r.Workload, cloud: r.Cloud, top: r}
			by[k] = w
		}
		w.savings += r.SavingsMonth
		if r.SavingsMonth > w.top.SavingsMonth {
			w.top = r
		}
		if r.Confidence == "low" {
			w.lowConfidence = true
		}
		if r.OOMKills > 0 || r.ThrottleRisk >= 0.05 {
			w.risky = true
		}
	}
	out := make([]*workloadWaste, 0, len(by))
	for _, w := range by {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].savings != out[j].savings {
			return out[i].savings > out[j].savings
		}
		return out[i].cluster+out[i].namespace+out[i].workload < out[j].cluster+out[j].namespace+out[j].workload
	})
	return out
}

func (w *workloadWaste) toProto(rank int) *kuberov1.WasteRecommendation {
	action, severity := "apply", "accent"
	if w.lowConfidence || w.risky {
		action, severity = "review", "warn"
	} else if w.top.Confidence != "high" {
		severity = "warn"
	}
	return &kuberov1.WasteRecommendation{
		Rank:                fmt.Sprintf("%02d", rank),
		Workload:            w.workload,
		Namespace:           w.namespace,
		Cluster:             w.cluster,
		Cloud:               managedName(w.cloud),
		Signal:              wasteSignal(w.top),
		RecoverableUsdMonth: math.Round(w.savings*100) / 100,
		Action:              action,
		Severity:            severity,
	}
}

// wasteSignal is the one-line evidence, e.g. "cpu.req=2.0 p95=0.31".
func wasteSignal(r rightsizing.Recommendation) string {
	if r.CPUSavingsMonth >= r.MemSavingsMonth {
		return fmt.Sprintf("cpu.req=%s p95=%s", oneDecimal(r.CPURequest), oneDecimal(r.CPUP95))
	}
	return fmt.Sprintf("mem.req=%s max=%s", k8sBytes(r.MemRequest), k8sBytes(r.MemMax))
}

func oneDecimal(v float64) string {
	s := strconv.FormatFloat(v, 'f', 2, 64)
	s = strings.TrimRight(s, "0")
	if strings.HasSuffix(s, ".") {
		s += "0"
	}
	return s
}

func k8sBytes(b float64) string {
	const gi, mi = 1 << 30, 1 << 20
	if b >= gi {
		return strconv.FormatFloat(math.Round(b/gi*10)/10, 'f', -1, 64) + "Gi"
	}
	return strconv.FormatFloat(math.Round(b/mi), 'f', -1, 64) + "Mi"
}

// managedName maps a cloud to the dashboard's managed-Kubernetes label.
func managedName(cloud string) string {
	switch strings.ToLower(cloud) {
	case "aws":
		return "EKS"
	case "gcp":
		return "GKE"
	case "azure":
		return "AKS"
	}
	return ""
}

func (c *ControlPlane) liveWaste(ctx context.Context, cluster string, limit int) ([]*kuberov1.WasteRecommendation, error) {
	w, err := timewin.Parse(wasteWindow, time.Now())
	if err != nil {
		return nil, err
	}
	recs, err := c.live().rightsizing.Recommend(ctx, rightsizing.Query{ClusterID: cluster, Window: w,
		Options: rightsizing.Options{WindowLabel: wasteWindow}})
	if err != nil {
		return nil, err
	}
	agg := wasteFromRecs(recs)
	if len(agg) > limit {
		agg = agg[:limit]
	}
	out := make([]*kuberov1.WasteRecommendation, 0, len(agg))
	for i, a := range agg {
		out = append(out, a.toProto(i+1))
	}
	return out, nil
}

// liveWorkload returns the workload's waste recommendation (nil when it
// has nothing recoverable) and its audit history, newest first.
func (c *ControlPlane) liveWorkload(ctx context.Context, cluster, namespace, name string) (*kuberov1.WasteRecommendation, []*kuberov1.AuditEntry, error) {
	w, err := timewin.Parse(wasteWindow, time.Now())
	if err != nil {
		return nil, nil, err
	}
	recs, err := c.live().rightsizing.Recommend(ctx, rightsizing.Query{ClusterID: cluster, Namespace: namespace,
		Workload: name, Window: w, Options: rightsizing.Options{WindowLabel: wasteWindow}})
	if err != nil {
		return nil, nil, err
	}
	var rec *kuberov1.WasteRecommendation
	if agg := wasteFromRecs(recs); len(agg) > 0 {
		rec = agg[0].toProto(1)
	}
	history, err := c.workloadHistory(ctx, cluster, namespace, name)
	if err != nil {
		return nil, nil, err
	}
	return rec, history, nil
}

const maxWorkloadHistory = 50

// workloadHistory filters the audit log to entries about one workload
// (target "name" or "namespace/name") in one cluster.
func (c *ControlPlane) workloadHistory(ctx context.Context, cluster, namespace, name string) ([]*kuberov1.AuditEntry, error) {
	if c.Audit == nil {
		return nil, nil
	}
	rows, err := c.Audit.List(ctx, "default", 500)
	if err != nil {
		return nil, fmt.Errorf("audit list: %w", err)
	}
	aliases := map[string]bool{}
	for _, a := range c.live().clusters.Snapshot(ctx).Aliases(cluster) {
		aliases[a] = true
	}
	var out []*kuberov1.AuditEntry
	for _, r := range rows {
		target := r.TargetName
		if target != name && target != namespace+"/"+name {
			continue
		}
		if r.ClusterID != nil && !aliases[*r.ClusterID] {
			continue
		}
		out = append(out, auditRowToProto(r))
		if len(out) >= maxWorkloadHistory {
			break
		}
	}
	return out, nil
}

// ─── ListPolicies ────────────────────────────────────────────────────

// livePolicies lists every mirrored policy with its monthly ceiling and
// month-to-date spend in its scope (see internal/budget for the scope
// rules).
func (c *ControlPlane) livePolicies(ctx context.Context, lister store.PolicyLister, cluster, kind string) ([]*kuberov1.Policy, error) {
	rows, err := lister.ListAll(ctx, cluster)
	if err != nil {
		return nil, err
	}
	budgets := map[[2]string]*store.PolicyRow{}
	for _, r := range rows {
		if r.Kind == "BudgetPolicy" {
			budgets[[2]string{r.ClusterID, r.Name}] = r
		}
	}
	var mtd map[[2]string]float64
	if c.CH != nil {
		if mtd, err = c.monthToDateByNamespace(ctx); err != nil {
			slog.Default().Warn("list policies: month-to-date spend unavailable", "err", err)
		}
	}
	snap := c.live().clusters.Snapshot(ctx)
	out := make([]*kuberov1.Policy, 0, len(rows))
	for _, r := range rows {
		if kind != "" && r.Kind != kind {
			continue
		}
		spec, err := budget.ParseSpec(r.SpecJSON)
		if err != nil {
			slog.Default().Warn("list policies: unreadable spec", "policy", r.Name, "err", err)
		}
		scopeSpec := spec
		if r.Kind == "CeilingPolicy" {
			if b := budgets[[2]string{r.ClusterID, spec.BudgetRef}]; b != nil {
				if bs, err := budget.ParseSpec(b.SpecJSON); err == nil {
					scopeSpec = bs
				}
			}
		}
		ceiling, _ := budget.ParseCeiling(scopeSpec.Ceiling)
		p := &kuberov1.Policy{
			Name:       r.Name,
			Scope:      scopeSpec.Describe(r.ClusterSlug),
			CeilingUsd: ceiling,
			Kind:       r.Kind,
			Armed:      r.Armed,
		}
		if r.Kind == "CeilingPolicy" && spec.BudgetRef != "" {
			p.Scope = "budget " + spec.BudgetRef + " · " + p.Scope
		}
		if ceiling > 0 && mtd != nil {
			spent := scopedSpend(mtd, snap.Aliases(r.ClusterID), scopeSpec)
			// Over-budget shows as >100: honest, the UI clamps its bar.
			p.SpentPct = int32(math.Min(math.Round(spent/ceiling*100), math.MaxInt32))
		}
		out = append(out, p)
	}
	return out, nil
}

func scopedSpend(mtd map[[2]string]float64, aliases []string, spec budget.Spec) float64 {
	ns, _ := spec.Namespaces()
	inScope := map[string]bool{}
	for _, n := range ns {
		inScope[n] = true
	}
	var spent float64
	for _, cl := range aliases {
		for k, v := range mtd {
			if k[0] == cl && (ns == nil || inScope[k[1]]) {
				spent += v
			}
		}
	}
	return spent
}

// monthToDateByNamespace returns this month's spend per (raw cluster
// id, namespace).
func (c *ControlPlane) monthToDateByNamespace(ctx context.Context) (map[[2]string]float64, error) {
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	qctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	rows, err := c.CH.QueryContext(qctx, `
		SELECT cluster_id, namespace, sum(cost_usd) FROM workload_cost_1h
		WHERE ts_hour >= toDateTime(?) GROUP BY cluster_id, namespace LIMIT 100000`, monthStart.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[[2]string]float64{}
	for rows.Next() {
		var (
			cl, ns string
			v      float64
		)
		if err := rows.Scan(&cl, &ns, &v); err != nil {
			return nil, err
		}
		out[[2]string{cl, ns}] = v
	}
	return out, rows.Err()
}

// ─── Capacity demands ────────────────────────────────────────────────

func (c *ControlPlane) liveCapacity(ctx context.Context, cluster string) ([]*kuberov1.CapacityDemand, error) {
	ds, err := c.live().insights.CapacityDemands(ctx, cluster, time.Now().UTC())
	if err != nil {
		return nil, err
	}
	out := make([]*kuberov1.CapacityDemand, 0, len(ds))
	for _, d := range ds {
		gpu := ""
		if d.GPUs > 0 {
			kind := d.GPUKind
			if kind == "" {
				kind = "GPU"
			}
			gpu = fmt.Sprintf("%s× %s", strconv.FormatFloat(d.GPUs, 'f', -1, 64), kind)
		}
		out = append(out, &kuberov1.CapacityDemand{
			Id: d.ID, Cluster: d.Cluster, Namespace: d.Namespace, Workload: d.Workload,
			PendingPods:             int32(d.PendingPods),
			RequestedCpu:            insights.FmtCores(d.CPUCores),
			RequestedMem:            insights.FmtBytes(d.MemBytes),
			RequestedGpu:            gpu,
			OldestPendingAge:        insights.FmtAge(d.OldestAge),
			RecommendedAction:       d.RecommendedAction,
			RecommendedCostUsdMonth: math.Round(d.RecommendedUSDMo*100) / 100,
			BlockedCostUsdMonth:     math.Round(d.BlockedUSDMo*100) / 100,
			Source:                  "clickhouse",
		})
	}
	return out, nil
}

// ─── Team spend ──────────────────────────────────────────────────────

// liveTeamSpend is chargeback per (team, cost center): allocated
// compute + network spend over the window scaled to a 30-day month,
// split by cloud; recoverable $ from the rightsizing engine; GPU idle
// where GPU utilisation telemetry exists.
func (c *ControlPlane) liveTeamSpend(ctx context.Context, window string) (*kuberov1.GetTeamSpendResponse, error) {
	now := time.Now().UTC()
	w, err := timewin.ParseDefault(window, "30d", now)
	if err != nil {
		return nil, errInvalid(err)
	}
	d := c.live()
	dims := []cost.Dim{{Name: cost.DimTeam}, {Name: cost.DimCostCenter}, {Name: cost.DimCloud},
		{Name: cost.DimCluster}, {Name: cost.DimNamespace}, {Name: cost.DimWorkload}}
	sets, err := d.cost.Allocate(ctx, cost.AllocationQuery{Window: w, Dims: dims})
	if err != nil {
		return nil, err
	}
	covered := w.Covered()
	type teamKey struct{ team, cc string }
	teams := map[teamKey]*kuberov1.TeamSpend{}
	get := func(k teamKey) *kuberov1.TeamSpend {
		t := teams[k]
		if t == nil {
			name := k.team
			if name == "" {
				name = cost.Unallocated
			}
			t = &kuberov1.TeamSpend{Team: name, CostCenter: k.cc}
			teams[k] = t
		}
		return t
	}
	// workload → (team, cost center) that carried most of its cost.
	type owner struct {
		k    teamKey
		cost float64
	}
	owners := map[[3]string]owner{}
	if len(sets) > 0 {
		for _, a := range sets[0].Rows {
			k := teamKey{a.Key[0], a.Key[1]}
			spend := timewin.PerMonth(a.Cost+a.NetCost, covered)
			t := get(k)
			t.SpendUsdMonth += spend
			switch strings.ToLower(a.Key[2]) {
			case "aws":
				t.AwsUsdMonth += spend
			case "gcp":
				t.GcpUsdMonth += spend
			case "azure":
				t.AzureUsdMonth += spend
			}
			wk := [3]string{a.Key[3], a.Key[4], a.Key[5]}
			if o, ok := owners[wk]; !ok || a.Cost > o.cost {
				owners[wk] = owner{k, a.Cost}
			}
		}
	}

	if idle, err := c.gpuIdle(ctx, w); err != nil {
		slog.Default().Warn("team spend: gpu idle unavailable", "err", err)
	} else {
		for k, v := range idle {
			get(teamKey{k[0], k[1]}).GpuIdleUsdMonth += timewin.PerMonth(v, covered)
		}
	}

	rw := w
	if rw.Duration() > 90*timewin.Day {
		rw.Start = rw.QueryEnd().Add(-90 * timewin.Day)
	}
	if recs, err := d.rightsizing.Recommend(ctx, rightsizing.Query{Window: rw}); err != nil {
		slog.Default().Warn("team spend: rightsizing unavailable", "err", err)
	} else {
		for wk, v := range rightsizing.PositiveSavingsByWorkload(downsizesOnly(recs)) {
			if o, ok := owners[wk]; ok {
				get(o.k).RecoverableUsdMonth += v
			}
		}
	}

	resp := &kuberov1.GetTeamSpendResponse{}
	for _, t := range teams {
		roundTeam(t)
		resp.Teams = append(resp.Teams, t)
		resp.FleetTotalUsdMonth += t.SpendUsdMonth
		resp.FleetRecoverableUsdMonth += t.RecoverableUsdMonth
	}
	sort.Slice(resp.Teams, func(i, j int) bool {
		if resp.Teams[i].SpendUsdMonth != resp.Teams[j].SpendUsdMonth {
			return resp.Teams[i].SpendUsdMonth > resp.Teams[j].SpendUsdMonth
		}
		return resp.Teams[i].Team < resp.Teams[j].Team
	})
	return resp, nil
}

func downsizesOnly(recs []rightsizing.Recommendation) []rightsizing.Recommendation {
	out := recs[:0:0]
	for _, r := range recs {
		if r.Direction == rightsizing.Downsize {
			out = append(out, r)
		}
	}
	return out
}

func roundTeam(t *kuberov1.TeamSpend) {
	r := func(v float64) float64 { return math.Round(v*100) / 100 }
	t.SpendUsdMonth, t.RecoverableUsdMonth, t.GpuIdleUsdMonth = r(t.SpendUsdMonth), r(t.RecoverableUsdMonth), r(t.GpuIdleUsdMonth)
	t.AwsUsdMonth, t.GcpUsdMonth, t.AzureUsdMonth = r(t.AwsUsdMonth), r(t.GcpUsdMonth), r(t.AzureUsdMonth)
}

// gpuIdle returns idle GPU $ per (team, cost center): GPU cost × (1 −
// utilisation) from raw samples. A workload whose GPU utilisation is
// never reported above 0 is treated as "no telemetry" (DCGM not
// installed) and contributes nothing — reporting its whole GPU bill as
// idle would be a guess, not a measurement. The cost is that a GPU
// that sat truly idle all window under working telemetry is
// under-reported, the conservative direction.
func (c *ControlPlane) gpuIdle(ctx context.Context, w timewin.Window) (map[[2]string]float64, error) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := c.CH.QueryContext(qctx, `
		SELECT team, cost_center, sum(idle) FROM (
			SELECT team, cost_center, cluster_id, namespace, workload,
			       sum(gpu_cost_usd_sec * interval_sec * (1 - least(toFloat64(gpu_util_pct), 100) / 100)) AS idle,
			       max(gpu_util_pct) AS max_util
			FROM pod_cost_1s
			WHERE ts >= ? AND ts < ? AND gpu_count > 0
			GROUP BY team, cost_center, cluster_id, namespace, workload
		) WHERE max_util > 0
		GROUP BY team, cost_center`, w.Start.UnixMilli(), w.QueryEnd().UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[[2]string]float64{}
	for rows.Next() {
		var (
			team, cc string
			v        float64
		)
		if err := rows.Scan(&team, &cc, &v); err != nil {
			return nil, err
		}
		out[[2]string{team, cc}] += v
	}
	return out, rows.Err()
}

// ─── Anomalies beyond spend ──────────────────────────────────────────

// signalAnomalies adds OOM-kill bursts (kind "capacity") and error-log
// spikes (kind "logs"). Each detector failing only drops its own cards.
func (c *ControlPlane) signalAnomalies(ctx context.Context, baseline time.Duration, z float64) []*kuberov1.Anomaly {
	now := time.Now().UTC()
	in := c.live().insights
	var out []*kuberov1.Anomaly
	if bursts, err := in.OOMBursts(ctx, now, insights.DefaultOOMBurst); err != nil {
		slog.Default().Warn("anomalies: oom bursts unavailable", "err", err)
	} else {
		for _, b := range bursts {
			sev := "warn"
			if b.Kills >= 10 {
				sev = "critical"
			}
			subject := b.Namespace + "/" + b.Workload
			out = append(out, &kuberov1.Anomaly{
				Id:      anomalyID("capacity", b.Cluster, b.Namespace, b.Workload),
				Kind:    "capacity",
				Title:   fmt.Sprintf("%s: %d OOM kills in the last hour", subject, b.Kills),
				Subject: subject,
				Detail: fmt.Sprintf("container %s OOM-killed %d× (last %s UTC) — memory limit too low or a leak; rightsizing never lowers memory after an OOM",
					orUnknown(b.Container), b.Kills, b.Last.Format("15:04")),
				ImpactUsdMonth: math.Round(b.ExposureUSDMonth*100) / 100,
				Severity:       sev,
				Source:         "clickhouse",
				LinkPath:       fmt.Sprintf("/workloads/%s/%s/%s", b.Cluster, b.Namespace, b.Workload),
			})
		}
	}
	if spikes, err := in.LogErrorSpikes(ctx, now, baseline, z); err != nil {
		slog.Default().Warn("anomalies: log spikes unavailable", "err", err)
	} else {
		for _, s := range spikes {
			sev := "warn"
			if math.IsInf(s.Z, 1) || s.Z >= 2*z {
				sev = "critical"
			}
			zText := "new errors from a silent baseline"
			if !math.IsInf(s.Z, 0) {
				zText = fmt.Sprintf("z=%.1f", s.Z)
			}
			subject := s.Namespace + "/" + s.Workload
			selector := fmt.Sprintf(`{cluster=%q,namespace=%q,workload=%q,level=~"error|fatal|critical|panic"}`, s.Cluster, s.Namespace, s.Workload)
			out = append(out, &kuberov1.Anomaly{
				Id:      anomalyID("logs", s.Cluster, s.Namespace, s.Workload),
				Kind:    "logs",
				Title:   fmt.Sprintf("%s error logs %.0f/h vs %.0f/h baseline", subject, s.CurrentPerHour, s.MeanPerHour),
				Subject: subject,
				Detail: fmt.Sprintf("%.0f error lines in the last hour vs baseline %.0f/h ±%.0f (%s)",
					s.CurrentPerHour, s.MeanPerHour, s.StdDevPerHour, zText),
				DeltaPct:       s.DeltaPct,
				ImpactUsdMonth: math.Round(s.ExposureUSDMonth*100) / 100,
				Severity:       sev,
				Source:         "clickhouse",
				LinkPath:       "/logs?query=" + url.QueryEscape(selector),
			})
		}
	}
	return out
}

// sortByImpact ranks anomalies of every kind by $ exposure.
func sortByImpact(all []*kuberov1.Anomaly) []*kuberov1.Anomaly {
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetImpactUsdMonth() > all[j].GetImpactUsdMonth() })
	return all
}

// requireFleet rejects cluster-scoped credentials on fleet-wide reads.
func requireFleet(ctx context.Context) error { return clusters.RequireFleet(ctx) }

func anomalyID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "anom-" + hex.EncodeToString(sum[:4])
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

// errInvalid wraps request-validation failures of the live paths.
type invalidArg struct{ error }

func errInvalid(err error) error { return invalidArg{err} }

func isInvalid(err error) bool {
	var ia invalidArg
	return errors.As(err, &ia)
}
