// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package focus exports allocated Kubernetes cost as a FinOps FOCUS 1.2
// CSV — the vendor-neutral billing format FinOps tools ingest — from
//
//	GET /api/v1/export/focus?window=30d&aggregate=workload
//
// One row per charge period (a UTC day) per resource. Parameters:
// window (default 30d, ≤ 366d), aggregate = workload (default) |
// namespace | cluster, idle = separate (default: one idle-capacity row
// per cluster-day) | share (weighted into the rows) | none,
// shareNamespaces, cluster_id.
//
// What the numbers are — KubeHero is not the invoice issuer:
//
//	BilledCost, EffectiveCost,   the ALLOCATED ESTIMATE: node price × the
//	ListCost, ContractedCost     pod's max(request, usage) share, as the
//	                             collector priced it. Identical in all
//	                             four columns because the node price is
//	                             already the effective rate KubeHero
//	                             knows; reconcile against your cloud
//	                             bill (the billed truth) upstream.
//	ConsumedQuantity/PricingQuantity  allocated core-hours
//	                             (max(request, usage)), unit Core-Hours.
//	InvoiceIssuerName            "KubeHero (allocated)" — never a real
//	                             invoice.
//	PricingCategory              from the node lifecycle: on-demand →
//	                             Standard, spot → Dynamic, savings plan /
//	                             committed → Committed.
//	Everything else              identity (cluster → SubAccount,
//	                             namespace/workload → Resource, Tags) —
//	                             exact, not estimated.
//
// Network spend (egress + cross-zone, attributed to the source
// workload) is a separate row with ServiceCategory "Networking". x_*
// columns carry KubeHero extensions: idle, shared, network, efficiency,
// resource hours, recoverable. Rows stream one day at a time; a failure
// mid-export aborts the response so a truncated file can't be mistaken
// for a complete one.
package focus

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Path is where the export is mounted.
const Path = "/api/v1/export/focus"

// Columns is the CSV header, in order.
var Columns = []string{
	"BilledCost", "EffectiveCost", "ListCost", "ContractedCost", "BillingCurrency",
	"BillingPeriodStart", "BillingPeriodEnd", "ChargePeriodStart", "ChargePeriodEnd",
	"ChargeCategory", "ChargeClass", "ChargeDescription", "ChargeFrequency",
	"ConsumedQuantity", "ConsumedUnit", "PricingQuantity", "PricingUnit", "PricingCategory",
	"ProviderName", "PublisherName", "InvoiceIssuerName",
	"RegionId", "RegionName", "AvailabilityZone",
	"ResourceId", "ResourceName", "ResourceType",
	"ServiceCategory", "ServiceName", "SubAccountId", "SubAccountName", "Tags",
	"x_CostSource", "x_CpuCost", "x_RamCost", "x_GpuCost", "x_NetworkCost", "x_IdleCost", "x_SharedCost",
	"x_CpuEfficiency", "x_RamEfficiency", "x_CpuCoreHours", "x_RamGiBHours", "x_GpuHours", "x_RecoverableCost",
}

const maxWindow = 366 * timewin.Day

// Handler serves the export.
type Handler struct {
	Cost     *cost.Service
	CH       *sql.DB // labels for Tags; nil = no labels
	Clusters *clusters.Resolver
	Log      *slog.Logger
	Now      func() time.Time
}

func (h *Handler) now() time.Time {
	if h.Now != nil {
		return h.Now().UTC()
	}
	return time.Now().UTC()
}

type aggregation struct {
	name string
	dims []cost.Dim
	kind string // FOCUS ResourceType
}

var aggregations = map[string]aggregation{
	"workload": {"workload", dimsOf(cost.DimCluster, cost.DimNamespace, cost.DimWorkload, cost.DimWorkloadKind,
		cost.DimTeam, cost.DimCostCenter, cost.DimCloud, cost.DimRegion, cost.DimZone, cost.DimLifecycle), "Kubernetes Workload"},
	"namespace": {"namespace", dimsOf(cost.DimCluster, cost.DimNamespace, cost.DimTeam, cost.DimCostCenter,
		cost.DimCloud, cost.DimRegion, cost.DimZone, cost.DimLifecycle), "Kubernetes Namespace"},
	"cluster": {"cluster", dimsOf(cost.DimCluster, cost.DimCloud, cost.DimRegion, cost.DimZone, cost.DimLifecycle), "Kubernetes Cluster"},
}

func dimsOf(names ...string) []cost.Dim {
	out := make([]cost.Dim, len(names))
	for i, n := range names {
		out[i] = cost.Dim{Name: n}
	}
	return out
}

type request struct {
	window    timewin.Window
	agg       aggregation
	idle      string // separate | share | none
	shared    []string
	clusterID string
}

func (h *Handler) parse(r *http.Request) (request, error) {
	var req request
	v := r.URL.Query()
	w, err := timewin.ParseDefault(v.Get("window"), "30d", h.now())
	if err != nil {
		return req, err
	}
	if w.Duration() > maxWindow {
		return req, fmt.Errorf("window longer than %d days", int(maxWindow/timewin.Day))
	}
	agg := strings.ToLower(strings.TrimSpace(v.Get("aggregate")))
	if agg == "" {
		agg = "workload"
	}
	a, ok := aggregations[agg]
	if !ok {
		return req, fmt.Errorf("aggregate must be workload, namespace or cluster, got %q", agg)
	}
	idle := strings.ToLower(strings.TrimSpace(v.Get("idle")))
	switch idle {
	case "":
		idle = "separate"
	case "separate", "share", "none":
	default:
		return req, errors.New("idle must be separate, share or none")
	}
	shared, err := cost.ParseSharedNamespaces([]string{v.Get("shareNamespaces")})
	if err != nil {
		return req, err
	}
	cluster, err := clusters.Scope(r.Context(), h.Clusters.Snapshot(r.Context()), strings.TrimSpace(v.Get("cluster_id")))
	if err != nil {
		return req, err
	}
	return request{window: w, agg: a, idle: idle, shared: shared, clusterID: cluster}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpauth.WriteError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	req, err := h.parse(r)
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "cluster-scoped") {
			status = http.StatusForbidden
		}
		httpauth.WriteError(w, status, err.Error())
		return
	}
	alloc, source, err := h.Cost.Allocator("FOCUS export")
	if err != nil {
		httpauth.WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	ctx := r.Context()
	var labels map[labelKey]map[string]string
	if source == cost.SourceLive && req.agg.name == "workload" && h.CH != nil {
		if labels, err = h.workloadLabels(ctx, req); err != nil && h.Log != nil {
			h.Log.Warn("focus export: labels unavailable, exporting without them", "err", err)
		}
	}
	snap := h.Clusters.Snapshot(ctx)

	end := req.window.QueryEnd()
	filename := fmt.Sprintf("kubehero-focus-%s-%s.csv", req.window.Start.Format("20060102"), end.Format("20060102"))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("X-KubeHero-Source", source)
	cw := csv.NewWriter(w)
	if err := cw.Write(Columns); err != nil {
		return
	}
	flusher, _ := w.(http.Flusher)

	for day := req.window.Start; day.Before(end); {
		next := day.Truncate(timewin.Day).Add(timewin.Day)
		if next.After(end) {
			next = end
		}
		q := cost.AllocationQuery{
			Window:    timewin.Window{Start: day, End: next, Now: req.window.Now},
			Dims:      req.agg.dims,
			ClusterID: req.clusterID, SharedNamespaces: req.shared,
			IncludeIdle: req.idle == "separate",
		}
		if req.idle == "share" {
			q.ShareIdle = cost.ShareIdleWeighted
		}
		sets, err := alloc.Allocate(ctx, q)
		if err != nil {
			if h.Log != nil {
				h.Log.Error("focus export aborted mid-stream", "day", day.Format(time.DateOnly), "err", err)
			}
			// Headers are gone: abort the connection so the client sees
			// a failed transfer, not a silently truncated CSV.
			panic(http.ErrAbortHandler)
		}
		for _, set := range sets {
			for _, a := range set.Rows {
				for _, rec := range Rows(a, req.agg, day, next, source, snap, labels) {
					if err := cw.Write(rec); err != nil {
						return
					}
				}
			}
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		day = next
	}
}

type labelKey struct{ cluster, namespace, workload string }

const (
	maxLabelsPerWorkload = 50
	maxLabelValueLen     = 256
)

// workloadLabels reads the latest pod labels per workload for Tags.
func (h *Handler) workloadLabels(ctx context.Context, req request) (map[labelKey]map[string]string, error) {
	var w chsql.Where
	if req.clusterID != "" {
		w.In("cluster_id", h.Clusters.Snapshot(ctx).Aliases(req.clusterID))
	}
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	rows, err := h.CH.QueryContext(qctx, `
		SELECT cluster_id, namespace, workload, argMax(labels, updated_at)
		FROM pod_metadata WHERE `+w.SQL()+`
		GROUP BY cluster_id, namespace, workload LIMIT 200000`, w.Args()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	snap := h.Clusters.Snapshot(ctx)
	out := map[labelKey]map[string]string{}
	for rows.Next() {
		var (
			c, ns, wl string
			lbl       map[string]string
		)
		if err := rows.Scan(&c, &ns, &wl, &lbl); err != nil {
			return nil, err
		}
		trimmed := map[string]string{}
		keys := make([]string, 0, len(lbl))
		for k := range lbl {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if len(trimmed) >= maxLabelsPerWorkload {
				break
			}
			v := lbl[k]
			if len(v) > maxLabelValueLen {
				v = v[:maxLabelValueLen]
			}
			trimmed[k] = v
		}
		out[labelKey{snap.Display(c), ns, wl}] = trimmed
	}
	return out, rows.Err()
}

// Rows renders one allocation as FOCUS rows: the compute row plus a
// Networking row when the allocation carries network spend.
func Rows(a *cost.Alloc, agg aggregation, start, end time.Time, source string, snap *clusters.Snapshot,
	labels map[labelKey]map[string]string) [][]string {
	val := func(name string) string {
		for i, d := range agg.dims {
			if d.Name == name && i < len(a.Key) {
				if a.Key[i] == cost.IdleName || a.Key[i] == cost.SharedName {
					return ""
				}
				return a.Key[i]
			}
		}
		return a.Props[name]
	}
	cluster := val(cost.DimCluster)
	if cluster == "" {
		cluster = a.Props[cost.DimCluster]
	}
	ns, wl, kind := val(cost.DimNamespace), val(cost.DimWorkload), val(cost.DimWorkloadKind)
	cloud, region, zone := val(cost.DimCloud), val(cost.DimRegion), val(cost.DimZone)

	var resourceID, resourceName, resourceType, desc string
	switch {
	case a.Idle:
		resourceID, resourceName, resourceType = cluster+"/"+cost.IdleName, cost.IdleName, "Kubernetes Idle Capacity"
		desc = "Unallocated node capacity (idle) in cluster " + cluster
	case strings.HasSuffix(a.Name, cost.SharedName):
		resourceID, resourceName, resourceType = cluster+"/"+cost.SharedName, cost.SharedName, "Kubernetes Shared Namespaces"
		desc = "Shared-namespace cost with no allocation to share it to in cluster " + cluster
	default:
		resourceType = agg.kind
		switch agg.name {
		case "workload":
			resourceID = cluster + "/" + ns + "/" + wl
			resourceName = wl
			desc = fmt.Sprintf("Kubernetes workload %s/%s allocated compute (CPU, memory, GPU) on %s", ns, orDash(wl), cluster)
		case "namespace":
			resourceID = cluster + "/" + ns
			resourceName = ns
			desc = fmt.Sprintf("Kubernetes namespace %s allocated compute (CPU, memory, GPU) on %s", ns, cluster)
		default:
			resourceID, resourceName = cluster, cluster
			desc = "Kubernetes cluster " + cluster + " allocated compute (CPU, memory, GPU)"
		}
	}

	tags := map[string]string{"k8s.cluster": cluster}
	if ns != "" {
		tags["k8s.namespace"] = ns
	}
	if wl != "" {
		tags["k8s.workload"] = wl
	}
	if kind != "" {
		tags["k8s.workload.kind"] = kind
	}
	if t := val(cost.DimTeam); t != "" {
		tags["kubehero.io/team"] = t
	}
	if cc := val(cost.DimCostCenter); cc != "" {
		tags["kubehero.io/cost-center"] = cc
	}
	for k, v := range labels[labelKey{cluster, ns, wl}] {
		if _, taken := tags[k]; !taken {
			tags[k] = v
		}
	}
	tagJSON, _ := json.Marshal(tags)

	subName := cluster
	if c, ok := snap.Lookup(cluster); ok && c.Name != "" {
		subName = c.Name
	}
	periodStart := time.Date(start.Year(), start.Month(), 1, 0, 0, 0, 0, time.UTC)
	costSource := "KubeHero allocated estimate"
	if source == cost.SourceDemo {
		costSource = "KubeHero demo data"
	}
	computeCost := a.Cost + a.SharedCost + a.IdleCost
	coreHours := a.CPUAllocCS / 3600
	base := func(billed float64, service, serviceName, description string, qty float64, unit, pricing string) []string {
		q := ""
		if unit != "" {
			q = f(qty)
		}
		return []string{
			f(billed), f(billed), f(billed), f(billed), "USD",
			ts(periodStart), ts(periodStart.AddDate(0, 1, 0)), ts(start), ts(end),
			"Usage", "", description, "Usage-Based",
			q, unit, q, unit, pricing,
			providerName(cloud), "KubeHero", "KubeHero (allocated)",
			region, region, zone,
			resourceID, resourceName, resourceType,
			service, serviceName, cluster, subName, string(tagJSON),
			costSource,
		}
	}
	compute := append(base(computeCost, "Compute", "Kubernetes", desc, coreHours, unitIf(coreHours > 0, "Core-Hours"),
		pricingCategory(val(cost.DimLifecycle))),
		f(a.CPUCost), f(a.RAMCost), f(a.GPUCost), f(0), f(a.IdleCost), f(a.SharedCost),
		f(a.CPUEfficiency()), f(a.RAMEfficiency()), f(coreHours), f(a.RAMAllocBS/3600/(1<<30)), f(a.GPUSec/3600), f(a.Recoverable))
	out := [][]string{compute}
	if a.NetCost > 0 {
		netDesc := fmt.Sprintf("Internet egress and cross-zone data transfer attributed to %s (source side)", resourceID)
		net := append(base(a.NetCost, "Networking", "Kubernetes Network", netDesc, 0, "", ""),
			f(0), f(0), f(0), f(a.NetCost), f(0), f(0), "", "", "", "", "", "")
		out = append(out, net)
	}
	return out
}

func unitIf(ok bool, unit string) string {
	if ok {
		return unit
	}
	return ""
}

func orDash(s string) string {
	if s == "" {
		return cost.Unallocated
	}
	return s
}

func f(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func providerName(cloud string) string {
	switch strings.ToLower(cloud) {
	case "aws":
		return "AWS"
	case "gcp":
		return "Google Cloud"
	case "azure":
		return "Microsoft Azure"
	}
	return "Kubernetes"
}

func pricingCategory(lifecycle string) string {
	switch strings.ToLower(lifecycle) {
	case "on-demand", "ondemand":
		return "Standard"
	case "spot", "preemptible":
		return "Dynamic"
	case "savings-plan", "committed", "reserved":
		return "Committed"
	}
	return ""
}
