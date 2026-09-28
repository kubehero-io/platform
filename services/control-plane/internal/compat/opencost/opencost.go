// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package opencost serves KubeHero's allocation engine in OpenCost's
// wire format, so OpenCost/Kubecost consumers — the OpenCost UI and
// plugin, Grafana JSON-API dashboards, FinOps scripts — work against a
// KubeHero control plane unchanged:
//
//	GET /allocation/compute   (and /allocation, /model/allocation[/compute])
//
// Parameters follow OpenCost's: window (required), aggregate (comma
// list of cluster, node, namespace, controllerKind, controller, pod,
// container, label:<key>; KubeHero's team, nodepool, zone, workload,
// cost_center also work), accumulate, step, includeIdle / idle,
// shareIdle + shareSplit (weighted|even), shareNamespaces and the
// filterClusters/filterNodes/filterNamespaces/filterControllerKinds/
// filterControllers/filterPods/filterContainers/filterLabels family.
// The v2 filter language (filter=…) is rejected rather than ignored —
// silently dropping a filter would return the wrong numbers.
//
// The response mirrors OpenCost's AllocationJSON field for field
// (core/pkg/opencost/allocation_json.go): numbers rounded to six
// decimals, NaN/Inf as null, one map of name → allocation per step.
// Quantities KubeHero doesn't collect (PVs, load balancers, limits) are
// reported as 0 rather than invented.
package opencost

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Paths the handler is mounted on.
var Paths = []string{"/allocation/compute", "/allocation", "/model/allocation", "/model/allocation/compute"}

// Handler serves the OpenCost allocation API.
type Handler struct {
	Cost     *cost.Service
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

// Response is OpenCost's envelope.
type Response struct {
	Code    int    `json:"code"`
	Status  string `json:"status"`
	Data    any    `json:"data"`
	Message string `json:"message,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// maxQueryLen bounds the raw query string.
const maxQueryLen = 8 << 10

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		httpauth.WriteError(w, http.StatusMethodNotAllowed, "use GET")
		return
	}
	if len(r.URL.RawQuery) > maxQueryLen {
		httpauth.WriteError(w, http.StatusRequestURITooLong, "query string too long")
		return
	}
	q, dims, err := h.parse(r)
	if err != nil {
		httpauth.WriteError(w, http.StatusBadRequest, err.Error())
		return
	}
	alloc, source, err := h.Cost.Allocator("OpenCost /allocation")
	if err != nil {
		httpauth.WriteError(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	sets, err := alloc.Allocate(r.Context(), q)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, cost.ErrTooManyRows) {
			status = http.StatusUnprocessableEntity
		}
		if h.Log != nil {
			h.Log.Warn("opencost allocation failed", "err", err)
		}
		httpauth.WriteError(w, status, err.Error())
		return
	}
	data := make([]map[string]*AllocationJSON, 0, len(sets))
	for _, s := range sets {
		data = append(data, renderSet(s, dims))
	}
	resp := Response{Code: http.StatusOK, Status: "success", Data: data}
	if source == cost.SourceDemo {
		resp.Warning = "demo data: ClickHouse is not configured on this KubeHero control plane"
		w.Header().Set("X-KubeHero-Source", "demo")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// openCostDims maps OpenCost aggregate names onto KubeHero dimensions.
var openCostDims = map[string]string{
	"cluster":        cost.DimCluster,
	"node":           cost.DimNode,
	"namespace":      cost.DimNamespace,
	"controllerkind": cost.DimWorkloadKind,
	"controller":     cost.DimController,
	"pod":            cost.DimPod,
	"container":      cost.DimContainer,
	// KubeHero extensions, accepted under their own names.
	"workload":    cost.DimWorkload,
	"team":        cost.DimTeam,
	"nodepool":    cost.DimNodepool,
	"zone":        cost.DimZone,
	"cost_center": cost.DimCostCenter,
	"costcenter":  cost.DimCostCenter,
}

func (h *Handler) parse(r *http.Request) (cost.AllocationQuery, []cost.Dim, error) {
	var q cost.AllocationQuery
	v := r.URL.Query()
	if v.Get("filter") != "" {
		return q, nil, errors.New("the v2 'filter' parameter is not supported; use filterNamespaces, filterClusters, filterLabels, …")
	}
	window := strings.TrimSpace(v.Get("window"))
	if window == "" {
		return q, nil, errors.New("missing required parameter 'window'")
	}
	win, err := timewin.Parse(window, h.now())
	if err != nil {
		return q, nil, fmt.Errorf("invalid 'window' parameter: %w", err)
	}

	var dimNames []string
	for _, a := range splitList(v.Get("aggregate")) {
		if key, ok := strings.CutPrefix(a, "label:"); ok {
			dimNames = append(dimNames, "label:"+key)
			continue
		}
		d, ok := openCostDims[strings.ToLower(a)]
		if !ok {
			return q, nil, fmt.Errorf("unsupported aggregate %q", a)
		}
		dimNames = append(dimNames, d)
	}
	if len(dimNames) == 0 {
		// OpenCost's unaggregated allocation key.
		dimNames = []string{cost.DimCluster, cost.DimNode, cost.DimNamespace, cost.DimPod, cost.DimContainer}
	}
	dims, err := cost.ParseAggregate(dimNames)
	if err != nil {
		return q, nil, err
	}

	accumulate, err := boolParam(v, "accumulate", false)
	if err != nil {
		return q, nil, err
	}
	var step time.Duration
	if s := v.Get("step"); s != "" && !accumulate {
		if step, err = timewin.ParseStep(s); err != nil {
			return q, nil, err
		}
	} else if !accumulate && win.Duration() > timewin.Day {
		step = timewin.Day
	}

	includeIdle, err := boolParam(v, "includeIdle", false)
	if err != nil {
		return q, nil, err
	}
	if v.Has("idle") {
		if includeIdle, err = boolParam(v, "idle", false); err != nil {
			return q, nil, err
		}
	}
	shareIdle, err := boolParam(v, "shareIdle", false)
	if err != nil {
		return q, nil, err
	}
	share := cost.ShareIdleNone
	if shareIdle {
		share = cost.ShareIdleWeighted
		switch strings.ToLower(v.Get("shareSplit")) {
		case "", "weighted":
		case "even":
			share = cost.ShareIdleEven
		default:
			return q, nil, errors.New("shareSplit must be weighted or even")
		}
	}
	shared, err := cost.ParseSharedNamespaces([]string{v.Get("shareNamespaces")})
	if err != nil {
		return q, nil, err
	}

	filters := map[string][]string{}
	add := func(dim string, vals []string) {
		if len(vals) > 0 {
			filters[dim] = append(filters[dim], vals...)
		}
	}
	add(cost.DimCluster, splitList(v.Get("filterClusters")))
	add(cost.DimNode, splitList(v.Get("filterNodes")))
	add(cost.DimNamespace, splitList(v.Get("filterNamespaces")))
	add(cost.DimPod, splitList(v.Get("filterPods")))
	add(cost.DimContainer, splitList(v.Get("filterContainers")))
	for _, k := range splitList(v.Get("filterControllerKinds")) {
		add(cost.DimWorkloadKind, []string{KindFromOpenCost(k)})
	}
	for _, c := range splitList(v.Get("filterControllers")) {
		if kind, name, ok := strings.Cut(c, ":"); ok {
			add(cost.DimController, []string{KindFromOpenCost(kind) + "/" + name})
		} else {
			add(cost.DimWorkload, []string{c})
		}
	}
	for _, l := range splitList(v.Get("filterLabels")) {
		key, val, ok := strings.Cut(l, ":")
		if !ok || key == "" {
			return q, nil, fmt.Errorf("filterLabels entries must be key:value, got %q", l)
		}
		add("label:"+strings.TrimSpace(key), []string{strings.TrimSpace(val)})
	}
	fs, err := cost.ParseFilters(filters)
	if err != nil {
		return q, nil, err
	}

	// Cluster-scoped credentials only ever see their own cluster.
	cluster, err := clusters.Scope(r.Context(), h.Clusters.Snapshot(r.Context()), "")
	if err != nil {
		return q, nil, err
	}
	q = cost.AllocationQuery{Window: win, Dims: dims, Filters: fs, ClusterID: cluster,
		IncludeIdle: includeIdle, ShareIdle: share, SharedNamespaces: shared, Step: step}
	return q, dims, nil
}

func boolParam(v url.Values, key string, def bool) (bool, error) {
	s := strings.TrimSpace(v.Get(key))
	if s == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("parameter %q must be a boolean", key)
	}
	return b, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Kubernetes kinds as KubeHero stores them vs OpenCost's lower-case.
var kinds = map[string]string{
	"deployment": "Deployment", "statefulset": "StatefulSet", "daemonset": "DaemonSet",
	"job": "Job", "cronjob": "CronJob", "replicaset": "ReplicaSet", "pod": "Pod",
	"replicationcontroller": "ReplicationController", "rollout": "Rollout",
}

// KindFromOpenCost maps "deployment" → "Deployment".
func KindFromOpenCost(k string) string {
	if v, ok := kinds[strings.ToLower(k)]; ok {
		return v
	}
	if k == "" {
		return k
	}
	return strings.ToUpper(k[:1]) + k[1:]
}

// AllocationProperties mirrors OpenCost's AllocationProperties.
type AllocationProperties struct {
	Cluster              string            `json:"cluster,omitempty"`
	Node                 string            `json:"node,omitempty"`
	Container            string            `json:"container,omitempty"`
	Controller           string            `json:"controller,omitempty"`
	ControllerKind       string            `json:"controllerKind,omitempty"`
	Namespace            string            `json:"namespace,omitempty"`
	Pod                  string            `json:"pod,omitempty"`
	Services             []string          `json:"services,omitempty"`
	ProviderID           string            `json:"providerID,omitempty"`
	Labels               map[string]string `json:"labels,omitempty"`
	Annotations          map[string]string `json:"annotations,omitempty"`
	NamespaceLabels      map[string]string `json:"namespaceLabels,omitempty"`
	NamespaceAnnotations map[string]string `json:"namespaceAnnotations,omitempty"`
}

// Window is OpenCost's {"start","end"} window.
type Window struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

// AllocationJSON mirrors OpenCost's AllocationJSON field for field and
// in order.
type AllocationJSON struct {
	Name                         string                `json:"name"`
	Properties                   *AllocationProperties `json:"properties"`
	Window                       Window                `json:"window"`
	Start                        string                `json:"start"`
	End                          string                `json:"end"`
	Minutes                      *float64              `json:"minutes"`
	CPUCores                     *float64              `json:"cpuCores"`
	CPUCoreRequestAverage        *float64              `json:"cpuCoreRequestAverage"`
	CPUCoreLimitAverage          *float64              `json:"cpuCoreLimitAverage"`
	CPUCoreUsageAverage          *float64              `json:"cpuCoreUsageAverage"`
	CPUCoreHours                 *float64              `json:"cpuCoreHours"`
	CPUCost                      *float64              `json:"cpuCost"`
	CPUCostAdjustment            *float64              `json:"cpuCostAdjustment"`
	CPUCostIdle                  *float64              `json:"cpuCostIdle"`
	CPUEfficiency                *float64              `json:"cpuEfficiency"`
	GPUCount                     *float64              `json:"gpuCount"`
	GPUHours                     *float64              `json:"gpuHours"`
	GPUCost                      *float64              `json:"gpuCost"`
	GPUCostAdjustment            *float64              `json:"gpuCostAdjustment"`
	GPUCostIdle                  *float64              `json:"gpuCostIdle"`
	GPUEfficiency                *float64              `json:"gpuEfficiency"`
	NetworkTransferBytes         *float64              `json:"networkTransferBytes"`
	NetworkReceiveBytes          *float64              `json:"networkReceiveBytes"`
	NetworkCost                  *float64              `json:"networkCost"`
	NetworkCrossZoneCost         *float64              `json:"networkCrossZoneCost"`
	NetworkCrossRegionCost       *float64              `json:"networkCrossRegionCost"`
	NetworkInternetCost          *float64              `json:"networkInternetCost"`
	NetworkNatGatewayEgressCost  *float64              `json:"networkNatGatewayEgressCost"`
	NetworkNatGatewayIngressCost *float64              `json:"networkNatGatewayIngressCost"`
	NetworkCostAdjustment        *float64              `json:"networkCostAdjustment"`
	LoadBalancerCost             *float64              `json:"loadBalancerCost"`
	LoadBalancerCostAdjustment   *float64              `json:"loadBalancerCostAdjustment"`
	PVBytes                      *float64              `json:"pvBytes"`
	PVByteHours                  *float64              `json:"pvByteHours"`
	PVCost                       *float64              `json:"pvCost"`
	PVs                          map[string]any        `json:"pvs"`
	PVCostAdjustment             *float64              `json:"pvCostAdjustment"`
	RAMBytes                     *float64              `json:"ramBytes"`
	RAMByteRequestAverage        *float64              `json:"ramByteRequestAverage"`
	RAMByteLimitAverage          *float64              `json:"ramByteLimitAverage"`
	RAMByteUsageAverage          *float64              `json:"ramByteUsageAverage"`
	RAMByteHours                 *float64              `json:"ramByteHours"`
	RAMCost                      *float64              `json:"ramCost"`
	RAMCostAdjustment            *float64              `json:"ramCostAdjustment"`
	RAMCostIdle                  *float64              `json:"ramCostIdle"`
	RAMEfficiency                *float64              `json:"ramEfficiency"`
	ExternalCost                 *float64              `json:"externalCost"`
	SharedCost                   *float64              `json:"sharedCost"`
	TotalCost                    *float64              `json:"totalCost"`
	TotalEfficiency              *float64              `json:"totalEfficiency"`
	LoadBalancers                map[string]any        `json:"lbAllocations"`
	GPUAllocation                map[string]any        `json:"gpuAllocation"`
}

// num is OpenCost's formatFloat64ForResponse: six decimals, NaN/Inf →
// null.
func num(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	v := math.Round(f*1e6) / 1e6
	return &v
}

// renderSet converts one step into OpenCost's name → allocation map.
// Idle is re-keyed OpenCost-style: "<cluster>/__idle__" when clusters
// are aggregated, one merged "__idle__" otherwise.
func renderSet(s cost.AllocSet, dims []cost.Dim) map[string]*AllocationJSON {
	out := make(map[string]*AllocationJSON, len(s.Rows))
	byCluster := false
	for _, d := range dims {
		if d.Name == cost.DimCluster {
			byCluster = true
		}
	}
	// Resource mix of the set, to split idle rows (whose capacity mix
	// isn't known per row) into cpu/ram/gpu the way the fleet spends.
	var mixCPU, mixRAM, mixGPU float64
	for _, a := range s.Rows {
		if !a.Idle {
			mixCPU += a.CPUCost
			mixRAM += a.RAMCost
			mixGPU += a.GPUCost
		}
	}
	for _, a := range s.Rows {
		name := keyName(a, dims)
		if a.Idle || strings.HasSuffix(a.Name, cost.SharedName) {
			suffix := cost.IdleName
			if !a.Idle {
				suffix = cost.SharedName
			}
			name = suffix
			if byCluster {
				name = a.Props[cost.DimCluster] + "/" + suffix
			}
		}
		j := toJSON(a, name, s, mixCPU, mixRAM, mixGPU)
		if prev, ok := out[name]; ok {
			out[name] = mergeJSON(prev, j)
			continue
		}
		out[name] = j
	}
	return out
}

// keyName composes the OpenCost allocation key for a row.
func keyName(a *cost.Alloc, dims []cost.Dim) string {
	parts := make([]string, len(dims))
	for i, d := range dims {
		v := ""
		if i < len(a.Key) {
			v = a.Key[i]
		}
		switch d.Name {
		case cost.DimController:
			if kind, n, ok := strings.Cut(v, "/"); ok {
				v = strings.ToLower(kind) + ":" + n
			}
		case cost.DimWorkloadKind:
			v = strings.ToLower(v)
		}
		if v == "" {
			v = cost.Unallocated
		}
		parts[i] = v
	}
	return strings.Join(parts, "/")
}

func toJSON(a *cost.Alloc, name string, s cost.AllocSet, mixCPU, mixRAM, mixGPU float64) *AllocationJSON {
	minutes := a.Minutes()
	hours := minutes / 60
	perHour := func(v float64) float64 {
		if hours <= 0 {
			return 0
		}
		return v / hours
	}
	perSec := func(v float64) float64 {
		if minutes <= 0 {
			return 0
		}
		return v / (minutes * 60)
	}
	cpuCost, ramCost, gpuCost := a.CPUCost, a.RAMCost, a.GPUCost
	var cpuIdle, ramIdle, gpuIdle float64
	if idle := a.IdleCost; idle > 0 {
		// OpenCost folds idle into the per-resource costs (and reports
		// the idle part in *CostIdle): split it by the row's own mix, or
		// the set's mix for an idle row.
		c, r, g := cpuCost, ramCost, gpuCost
		if a.Idle || c+r+g <= 0 {
			c, r, g = mixCPU, mixRAM, mixGPU
		}
		if tot := c + r + g; tot > 0 {
			cpuIdle, ramIdle, gpuIdle = idle*c/tot, idle*r/tot, idle*g/tot
		} else {
			cpuIdle = idle
		}
		cpuCost += cpuIdle
		ramCost += ramIdle
		gpuCost += gpuIdle
	}
	props := &AllocationProperties{}
	for k, v := range a.Props {
		switch k {
		case cost.DimCluster:
			props.Cluster = v
		case cost.DimNamespace:
			props.Namespace = v
		case cost.DimNode:
			props.Node = v
		case cost.DimPod:
			props.Pod = v
		case cost.DimContainer:
			props.Container = v
		case cost.DimWorkload:
			if props.Controller == "" {
				props.Controller = v
			}
		case cost.DimWorkloadKind:
			props.ControllerKind = strings.ToLower(v)
		case cost.DimController:
			if kind, n, ok := strings.Cut(v, "/"); ok {
				props.ControllerKind = strings.ToLower(kind)
				props.Controller = n
			}
		default:
			if key, ok := strings.CutPrefix(k, "label:"); ok && v != "" {
				if props.Labels == nil {
					props.Labels = map[string]string{}
				}
				props.Labels[key] = v
			}
		}
	}
	zero := num(0)
	return &AllocationJSON{
		Name:                         name,
		Properties:                   props,
		Window:                       Window{Start: rfc(s.Start), End: rfc(s.End)},
		Start:                        rfc(a.Start),
		End:                          rfc(a.End),
		Minutes:                      num(minutes),
		CPUCores:                     num(perHour(a.CPUAllocCS / 3600)),
		CPUCoreRequestAverage:        num(perSec(a.CPUReqCS)),
		CPUCoreLimitAverage:          zero,
		CPUCoreUsageAverage:          num(perSec(a.CPUUseCS)),
		CPUCoreHours:                 num(a.CPUAllocCS / 3600),
		CPUCost:                      num(cpuCost),
		CPUCostAdjustment:            zero,
		CPUCostIdle:                  num(cpuIdle),
		CPUEfficiency:                num(a.CPUEfficiency()),
		GPUCount:                     num(perHour(a.GPUSec / 3600)),
		GPUHours:                     num(a.GPUSec / 3600),
		GPUCost:                      num(gpuCost),
		GPUCostAdjustment:            zero,
		GPUCostIdle:                  num(gpuIdle),
		GPUEfficiency:                zero,
		NetworkTransferBytes:         zero,
		NetworkReceiveBytes:          zero,
		NetworkCost:                  num(a.NetCost),
		NetworkCrossZoneCost:         num(a.NetCrossZoneCost),
		NetworkCrossRegionCost:       zero,
		NetworkInternetCost:          num(a.NetInternetCost),
		NetworkNatGatewayEgressCost:  zero,
		NetworkNatGatewayIngressCost: zero,
		NetworkCostAdjustment:        zero,
		LoadBalancerCost:             zero,
		LoadBalancerCostAdjustment:   zero,
		PVBytes:                      zero,
		PVByteHours:                  zero,
		PVCost:                       zero,
		PVCostAdjustment:             zero,
		RAMBytes:                     num(perHour(a.RAMAllocBS / 3600)),
		RAMByteRequestAverage:        num(perSec(a.RAMReqBS)),
		RAMByteLimitAverage:          zero,
		RAMByteUsageAverage:          num(perSec(a.RAMUseBS)),
		RAMByteHours:                 num(a.RAMAllocBS / 3600),
		RAMCost:                      num(ramCost),
		RAMCostAdjustment:            zero,
		RAMCostIdle:                  num(ramIdle),
		RAMEfficiency:                num(a.RAMEfficiency()),
		ExternalCost:                 zero,
		SharedCost:                   num(a.SharedCost),
		TotalCost:                    num(a.TotalCost()),
		TotalEfficiency:              num(a.TotalEfficiency()),
	}
}

// mergeJSON folds per-cluster idle rows into one "__idle__" entry.
func mergeJSON(a, b *AllocationJSON) *AllocationJSON {
	add := func(x, y *float64) *float64 {
		switch {
		case x == nil:
			return y
		case y == nil:
			return x
		}
		return num(*x + *y)
	}
	a.CPUCost = add(a.CPUCost, b.CPUCost)
	a.CPUCostIdle = add(a.CPUCostIdle, b.CPUCostIdle)
	a.RAMCost = add(a.RAMCost, b.RAMCost)
	a.RAMCostIdle = add(a.RAMCostIdle, b.RAMCostIdle)
	a.GPUCost = add(a.GPUCost, b.GPUCost)
	a.GPUCostIdle = add(a.GPUCostIdle, b.GPUCostIdle)
	a.SharedCost = add(a.SharedCost, b.SharedCost)
	a.TotalCost = add(a.TotalCost, b.TotalCost)
	if a.Properties != nil && b.Properties != nil && a.Properties.Cluster != b.Properties.Cluster {
		a.Properties.Cluster = ""
	}
	return a
}

func rfc(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// SortedNames returns a set's keys in a stable order (tests, CSV).
func SortedNames(m map[string]*AllocationJSON) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
