// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// metrics are the additive quantities of an allocation. Resources are
// kept as integrals (core-seconds, byte-seconds) so any set of rows
// sums exactly; averages and hours are derived at the edge.
type metrics struct {
	Cost        float64 // compute $ (cost_usd): CPU + RAM + GPU
	CPUCost     float64
	RAMCost     float64
	GPUCost     float64
	Recoverable float64

	CPUReqCS   float64 // requested core-seconds
	CPUUseCS   float64 // used core-seconds
	CPUAllocCS float64 // billed core-seconds: max(request, usage)
	RAMReqBS   float64
	RAMUseBS   float64
	RAMAllocBS float64
	GPUSec     float64
	PodSec     float64

	NetCost          float64 // egress + cross-zone $ attributed to the source
	NetCrossZoneCost float64
	NetInternetCost  float64
	LogBytes         float64

	SharedCost float64 // redistributed shared-namespace $
	IdleCost   float64 // shared idle $ (or the whole idle row)
}

func (m *metrics) add(o *metrics) {
	m.Cost += o.Cost
	m.CPUCost += o.CPUCost
	m.RAMCost += o.RAMCost
	m.GPUCost += o.GPUCost
	m.Recoverable += o.Recoverable
	m.CPUReqCS += o.CPUReqCS
	m.CPUUseCS += o.CPUUseCS
	m.CPUAllocCS += o.CPUAllocCS
	m.RAMReqBS += o.RAMReqBS
	m.RAMUseBS += o.RAMUseBS
	m.RAMAllocBS += o.RAMAllocBS
	m.GPUSec += o.GPUSec
	m.PodSec += o.PodSec
	m.NetCost += o.NetCost
	m.NetCrossZoneCost += o.NetCrossZoneCost
	m.NetInternetCost += o.NetInternetCost
	m.LogBytes += o.LogBytes
	m.SharedCost += o.SharedCost
	m.IdleCost += o.IdleCost
}

// TotalCost is everything the row is charged: compute + network +
// redistributed shared namespaces + shared idle (PV cost is not yet
// collected, so it contributes 0).
func (m *metrics) TotalCost() float64 {
	return m.Cost + m.NetCost + m.SharedCost + m.IdleCost
}

// normalizeSplit makes CPU + RAM + GPU add up to Cost. Collectors that
// predate the per-resource split report only cost_usd; the cost model
// blends CPU share and memory share 50/50, so an unsplit remainder is
// assigned half to CPU and half to RAM. An over-split (rounding in the
// collector) is scaled down proportionally. Cost stays the source of
// truth because idle and totals are computed from it.
func (m *metrics) normalizeSplit() {
	parts := m.CPUCost + m.RAMCost + m.GPUCost
	rem := m.Cost - parts
	switch {
	case math.Abs(rem) <= 1e-12*math.Max(1, math.Abs(m.Cost)):
	case rem > 0:
		m.CPUCost += rem / 2
		m.RAMCost += rem / 2
	case parts > 0:
		f := m.Cost / parts
		m.CPUCost *= f
		m.RAMCost *= f
		m.GPUCost *= f
	}
}

// Efficiency returns usage/request the way OpenCost defines it: 0 with
// no usage, 1 when there is usage but no request.
func efficiency(use, req float64) float64 {
	switch {
	case req > 0:
		return use / req
	case use > 0:
		return 1
	}
	return 0
}

// CPUEfficiency / RAMEfficiency / TotalEfficiency follow OpenCost:
// total = (cpuEff·cpuCost + ramEff·ramCost) / (cpuCost + ramCost).
func (m *metrics) CPUEfficiency() float64 { return efficiency(m.CPUUseCS, m.CPUReqCS) }

// RAMEfficiency is used / requested byte-seconds.
func (m *metrics) RAMEfficiency() float64 { return efficiency(m.RAMUseBS, m.RAMReqBS) }

// TotalEfficiency blends CPU and RAM efficiency by cost.
func (m *metrics) TotalEfficiency() float64 {
	if d := m.CPUCost + m.RAMCost; d > 0 {
		return (m.CPUEfficiency()*m.CPUCost + m.RAMEfficiency()*m.RAMCost) / d
	}
	return 0
}

// fineRow is the unit the query layer produces: one (bucket, cluster,
// namespace, workload, requested-dims…) combination. Aggregation to
// the requested dimensions, sharing and idle all happen on these so
// every per-cluster rule stays exact even when an output row spans
// clusters.
type fineRow struct {
	Bucket    int
	Cluster   string // display name
	Namespace string
	Workload  string
	Vals      []string // aligned with the query's dims
	Start     int64    // unix seconds; 0 = unknown
	End       int64
	M         metrics
}

type clusterKey struct {
	Bucket  int
	Cluster string
}

// clusterTotals are the unfiltered per-cluster denominators used when
// filters hide part of the cluster.
type clusterTotals struct {
	NonShared float64 // compute $ outside shared namespaces
	Shared    float64 // compute $ inside shared namespaces
}

// Alloc is one output allocation.
type Alloc struct {
	Name  string
	Key   []string          // dimension values in aggregate order ("" = unallocated)
	Props map[string]string // properties that are unique across the row's members
	Idle  bool
	Start time.Time
	End   time.Time
	metrics

	clusters map[string]bool
	conflict map[string]bool
}

// Minutes is the row's covered time.
func (a *Alloc) Minutes() float64 { return a.End.Sub(a.Start).Minutes() }

// AllocSet is one step of an allocation result.
type AllocSet struct {
	Start time.Time
	End   time.Time
	Rows  []*Alloc
	Total *Alloc
}

type bucketWindow struct{ Start, End time.Time }

// aggInput is everything aggregate needs; the live and demo paths both
// build one.
type aggInput struct {
	Dims    []Dim
	Rows    []fineRow
	Buckets []bucketWindow
	// Totals holds unfiltered per-cluster denominators; nil means the
	// rows are the whole cluster (no non-cluster filters) and the
	// denominators are derived from them.
	Totals map[clusterKey]*clusterTotals
	// NodeCost is node spend per (bucket, cluster); nil = idle unknown.
	NodeCost    map[clusterKey]float64
	Shared      map[string]bool
	IncludeIdle bool
	ShareIdle   string
}

// aggregate folds fine rows into the requested dimensions and applies
// shared-namespace redistribution and idle handling:
//
//	T_c = compute $ of non-shared namespaces in cluster c (unfiltered)
//	S_c = compute $ of shared namespaces in cluster c (unfiltered)
//	Idle_c = max(0, node $ − T_c − S_c)
//	row.shared = S_c · row.cost / T_c
//	weighted:  row.idle = Idle_c · row.cost / T_c
//	even:      Idle_c · F_c/T_c split equally across the result's rows
//	           in c (F_c = the filtered rows' cost; F_c = T_c unfiltered)
//	separate:  one idle row per cluster worth Idle_c · F_c / T_c
//
// With filters, only the filtered rows' proportional share of idle and
// shared cost is shown — the same "idle filtration coefficient" OpenCost
// applies — so totals never include cost the filter excluded.
func aggregate(in aggInput) []AllocSet {
	totals := in.Totals
	filtered := totals != nil
	if !filtered {
		totals = map[clusterKey]*clusterTotals{}
		for i := range in.Rows {
			r := &in.Rows[i]
			ct := totals[clusterKey{r.Bucket, r.Cluster}]
			if ct == nil {
				ct = &clusterTotals{}
				totals[clusterKey{r.Bucket, r.Cluster}] = ct
			}
			if in.Shared[r.Namespace] {
				ct.Shared += r.M.Cost
			} else {
				ct.NonShared += r.M.Cost
			}
		}
	}

	idle := map[clusterKey]float64{}
	if in.NodeCost != nil && (in.IncludeIdle || in.ShareIdle != ShareIdleNone) {
		for k, node := range in.NodeCost {
			ct := totals[k]
			var used float64
			if ct != nil {
				used = ct.NonShared + ct.Shared
			}
			if v := node - used; v > 0 {
				idle[k] = v
			}
		}
	}

	// filteredCost: F_c, the cost of the rows actually in the result.
	filteredCost := map[clusterKey]float64{}
	for i := range in.Rows {
		r := &in.Rows[i]
		if !in.Shared[r.Namespace] {
			filteredCost[clusterKey{r.Bucket, r.Cluster}] += r.M.Cost
		}
	}

	sets := make([]AllocSet, len(in.Buckets))
	for i, b := range in.Buckets {
		sets[i] = AllocSet{Start: b.Start, End: b.End}
	}
	outByKey := map[string]*Alloc{}
	outBucket := map[*Alloc]int{}

	for i := range in.Rows {
		r := &in.Rows[i]
		if in.Shared[r.Namespace] || r.Bucket < 0 || r.Bucket >= len(sets) {
			continue
		}
		ck := clusterKey{r.Bucket, r.Cluster}
		m := r.M
		if ct := totals[ck]; ct != nil && ct.NonShared > 0 {
			m.SharedCost += ct.Shared * r.M.Cost / ct.NonShared
			if in.ShareIdle == ShareIdleWeighted {
				m.IdleCost += idle[ck] * r.M.Cost / ct.NonShared
			}
		}
		key := bucketKey(r.Bucket, r.Vals)
		a := outByKey[key]
		if a == nil {
			a = &Alloc{
				Key:      append([]string(nil), r.Vals...),
				Props:    map[string]string{},
				clusters: map[string]bool{},
				conflict: map[string]bool{},
			}
			a.Name = joinName(a.Key)
			outByKey[key] = a
			outBucket[a] = r.Bucket
			sets[r.Bucket].Rows = append(sets[r.Bucket].Rows, a)
		}
		a.metrics.add(&m)
		a.clusters[r.Cluster] = true
		a.setProp(DimCluster, r.Cluster)
		a.setProp(DimNamespace, r.Namespace)
		a.setProp(DimWorkload, r.Workload)
		for j, d := range in.Dims {
			if j < len(r.Vals) {
				a.setProp(d.Name, r.Vals[j])
			}
		}
		a.widen(r.Start, r.End)
	}

	// Even idle: the result's share of each cluster's idle, split
	// equally across the result rows that touch that cluster.
	if in.ShareIdle == ShareIdleEven {
		members := map[clusterKey][]*Alloc{}
		for _, a := range outByKey {
			for c := range a.clusters {
				k := clusterKey{outBucket[a], c}
				members[k] = append(members[k], a)
			}
		}
		for k, rows := range members {
			ct := totals[k]
			if ct == nil || ct.NonShared <= 0 || idle[k] <= 0 {
				continue
			}
			portion := idle[k] * filteredCost[k] / ct.NonShared
			for _, a := range rows {
				a.IdleCost += portion / float64(len(rows))
			}
		}
	}

	// Separate idle rows; also where sharing is impossible because the
	// cluster has no non-shared allocation to carry it.
	multi := clusterCount(in.NodeCost) > 1
	for k, v := range idle {
		ct := totals[k]
		shareable := ct != nil && ct.NonShared > 0
		var amount float64
		switch {
		case in.ShareIdle != ShareIdleNone && shareable:
			continue // already distributed
		case filtered && shareable:
			if filteredCost[k] <= 0 {
				continue
			}
			amount = v * filteredCost[k] / ct.NonShared
		case filtered:
			continue // nothing of this cluster survived the filter
		case in.IncludeIdle || in.ShareIdle != ShareIdleNone:
			amount = v
		}
		if amount <= 0 || k.Bucket < 0 || k.Bucket >= len(sets) {
			continue
		}
		name := IdleName
		if multi {
			name = k.Cluster + "/" + IdleName
		}
		key := make([]string, len(in.Dims))
		for j, d := range in.Dims {
			if d.Name == DimCluster {
				key[j] = k.Cluster
			} else {
				key[j] = IdleName
			}
		}
		a := &Alloc{
			Name:  name,
			Key:   key,
			Props: map[string]string{DimCluster: k.Cluster},
			Idle:  true,
			Start: sets[k.Bucket].Start,
			End:   sets[k.Bucket].End,
		}
		a.IdleCost = amount
		sets[k.Bucket].Rows = append(sets[k.Bucket].Rows, a)
	}

	// A cluster whose only cost sits in shared namespaces has nobody to
	// share it with; keep it visible rather than dropping it from totals.
	if !filtered && len(in.Shared) > 0 {
		for k, ct := range totals {
			if ct.NonShared > 0 || ct.Shared <= 0 || k.Bucket < 0 || k.Bucket >= len(sets) {
				continue
			}
			a := &Alloc{
				Name:  k.Cluster + "/" + SharedName,
				Key:   make([]string, len(in.Dims)),
				Props: map[string]string{DimCluster: k.Cluster},
				Start: sets[k.Bucket].Start,
				End:   sets[k.Bucket].End,
			}
			for j, d := range in.Dims {
				if d.Name == DimCluster {
					a.Key[j] = k.Cluster
				} else {
					a.Key[j] = SharedName
				}
			}
			a.SharedCost = ct.Shared
			sets[k.Bucket].Rows = append(sets[k.Bucket].Rows, a)
		}
	}

	for i := range sets {
		s := &sets[i]
		total := &Alloc{Name: "total", Props: map[string]string{}, Start: s.Start, End: s.End}
		for _, a := range s.Rows {
			if !a.Idle {
				a.clamp(s.Start, s.End)
			}
			for p := range a.conflict {
				delete(a.Props, p)
			}
			for p, v := range a.Props {
				if v == "" {
					delete(a.Props, p)
				}
			}
			total.metrics.add(&a.metrics)
		}
		sort.Slice(s.Rows, func(x, y int) bool {
			tx, ty := s.Rows[x].TotalCost(), s.Rows[y].TotalCost()
			if tx != ty {
				return tx > ty
			}
			return s.Rows[x].Name < s.Rows[y].Name
		})
		s.Total = total
	}
	return sets
}

func (a *Alloc) setProp(k, v string) {
	if a.conflict[k] {
		return
	}
	if old, ok := a.Props[k]; ok && old != v {
		a.conflict[k] = true
		return
	}
	a.Props[k] = v
}

func (a *Alloc) widen(start, end int64) {
	if start > 0 {
		s := time.Unix(start, 0).UTC()
		if a.Start.IsZero() || s.Before(a.Start) {
			a.Start = s
		}
	}
	if end > 0 {
		e := time.Unix(end, 0).UTC()
		if e.After(a.End) {
			a.End = e
		}
	}
}

// clamp bounds the row's observed span to its step window; rows with
// no timing info cover the whole step.
func (a *Alloc) clamp(start, end time.Time) {
	if a.Start.IsZero() || a.Start.Before(start) {
		a.Start = start
	}
	if a.End.IsZero() || a.End.After(end) {
		a.End = end
	}
	if a.End.Before(a.Start) {
		a.End = a.Start
	}
}

func bucketKey(b int, vals []string) string {
	var sb strings.Builder
	sb.WriteString(strconv.Itoa(b))
	for _, v := range vals {
		sb.WriteByte(0)
		sb.WriteString(v)
	}
	return sb.String()
}

func joinName(vals []string) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = displayValue(v)
	}
	return strings.Join(parts, "/")
}

func clusterCount(m map[clusterKey]float64) int {
	seen := map[string]bool{}
	for k := range m {
		seen[k.Cluster] = true
	}
	return len(seen)
}
