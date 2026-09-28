// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package cost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// ErrTooManyRows is returned when an aggregation would materialise
// more rows than the engine allows — narrow the window or filter.
var ErrTooManyRows = errors.New("allocation too fine-grained: too many rows; narrow the window, add filters or aggregate by coarser dimensions")

// Engine computes cost allocations from ClickHouse.
//
// Query plan: whole hours come from the workload_cost_1h rollup; the
// partial hours at either edge of a window that doesn't sit on the
// hour grid come from raw pod_cost_1s, so totals are exact to the
// second. Pod- and node-grained requests (the rollup has neither
// column) and windows under two hours read pod_cost_1s throughout.
type Engine struct {
	CH       *sql.DB
	Clusters *clusters.Resolver
	Log      *slog.Logger
	// Timeout bounds every ClickHouse query (default 30s).
	Timeout time.Duration
	// MaxRows caps the fine-grained rows one query may return
	// (default 200k) — the engine's memory bound.
	MaxRows int
}

func (e *Engine) timeout() time.Duration {
	if e.Timeout > 0 {
		return e.Timeout
	}
	return 30 * time.Second
}

func (e *Engine) maxRows() int {
	if e.MaxRows > 0 {
		return e.MaxRows
	}
	return 200_000
}

// AllocationQuery is a validated allocation request.
type AllocationQuery struct {
	Window           timewin.Window
	Dims             []Dim
	Filters          []Filter
	ClusterID        string
	IncludeIdle      bool
	ShareIdle        string
	SharedNamespaces []string
	// Step splits the window into sets (OpenCost accumulate=false).
	// 0 = one set. Step mode aligns the window start to the hour.
	Step time.Duration
}

const maxSteps = 1000

type source int

const (
	srcRollup source = iota
	srcRaw
)

type segment struct {
	src      source
	from, to time.Time
}

// planSegments splits [start, end) into rollup and raw pieces. When end
// is "now" the rollup's current (partial) hour holds exactly the data
// up to now, so only a past, unaligned end needs a raw tail.
func planSegments(start, end, now time.Time, forceRaw bool) []segment {
	if !end.After(start) {
		return nil
	}
	if forceRaw || end.Sub(start) < 2*time.Hour {
		return []segment{{srcRaw, start, end}}
	}
	var out []segment
	midFrom := timewin.CeilHour(start)
	if midFrom.After(start) {
		out = append(out, segment{srcRaw, start, midFrom})
	}
	midTo := end
	var tail *segment
	if end.Before(now) {
		if f := timewin.FloorHour(end); f.Before(end) {
			midTo = f
			tail = &segment{srcRaw, f, end}
		}
	} else {
		midTo = timewin.CeilHour(end)
	}
	if midTo.After(midFrom) {
		out = append(out, segment{srcRollup, midFrom, midTo})
	}
	if tail != nil {
		out = append(out, *tail)
	}
	return out
}

// bucketing maps timestamps to step indexes.
type bucketing struct {
	startSec int64
	stepSec  int64 // 0 = single bucket
}

// expr renders the bucket index for a unix-seconds SQL expression.
func (b bucketing) expr(secExpr string) (string, []any) {
	if b.stepSec <= 0 {
		return "toInt64(0)", nil
	}
	return "intDiv(" + secExpr + " - ?, ?)", []any{b.startSec, b.stepSec}
}

// buckets computes the step windows for q.
func (q *AllocationQuery) buckets() ([]bucketWindow, bucketing, error) {
	w := q.Window
	end := w.QueryEnd()
	if q.Step <= 0 {
		return []bucketWindow{{Start: w.Start, End: w.End}}, bucketing{}, nil
	}
	start := timewin.FloorHour(w.Start)
	n := int((end.Sub(start) + q.Step - 1) / q.Step)
	if n <= 0 {
		n = 1
	}
	if n > maxSteps {
		return nil, bucketing{}, fmt.Errorf("window/step yields %d steps (max %d)", n, maxSteps)
	}
	out := make([]bucketWindow, n)
	for i := range out {
		bs := start.Add(time.Duration(i) * q.Step)
		be := bs.Add(q.Step)
		if be.After(w.End) {
			be = w.End
		}
		out[i] = bucketWindow{Start: bs, End: be}
	}
	return out, bucketing{startSec: start.Unix(), stepSec: int64(q.Step / time.Second)}, nil
}

// Allocate runs the allocation query.
func (e *Engine) Allocate(ctx context.Context, q AllocationQuery) ([]AllocSet, error) {
	if e == nil || e.CH == nil {
		return nil, errors.New("cost engine: ClickHouse not configured")
	}
	bws, bk, err := q.buckets()
	if err != nil {
		return nil, err
	}
	start := q.Window.Start
	if q.Step > 0 {
		start = timewin.FloorHour(start)
	}
	end := q.Window.QueryEnd()
	snap := e.Clusters.Snapshot(ctx)

	p := newPlan(q, snap)
	segs := planSegments(start, end, q.Window.Now, p.forceRaw)
	needIdle := q.IncludeIdle || q.ShareIdle != ShareIdleNone

	var (
		mu       sync.Mutex
		rows     []fineRow
		totals   map[clusterKey]*clusterTotals
		nodeCost map[clusterKey]float64
		net      = map[wlKey]*metrics{}
		logs     = map[wlKey]float64{}
		shares   map[wlID][]containerShare
	)
	if p.filtered {
		totals = map[clusterKey]*clusterTotals{}
	}
	if needIdle {
		nodeCost = map[clusterKey]float64{}
	}

	var jobs []func(context.Context) error
	for _, seg := range segs {
		seg := seg
		jobs = append(jobs, func(ctx context.Context) error {
			got, err := e.fineRows(ctx, p, seg, bk, snap)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			rows = append(rows, got...)
			if len(rows) > e.maxRows() {
				return ErrTooManyRows
			}
			return nil
		})
		if p.filtered {
			jobs = append(jobs, func(ctx context.Context) error {
				return e.clusterTotals(ctx, p, seg, bk, snap, &mu, totals)
			})
		}
		if needIdle {
			jobs = append(jobs, func(ctx context.Context) error {
				return e.nodeCost(ctx, p, seg, bk, snap, &mu, nodeCost)
			})
		}
		jobs = append(jobs, func(ctx context.Context) error {
			return e.networkCost(ctx, p, seg, bk, snap, &mu, net)
		})
	}
	jobs = append(jobs, func(ctx context.Context) error {
		return e.logVolume(ctx, p, start, end, bk, snap, &mu, logs)
	})
	if p.containerDim >= 0 || p.containerFilter != nil {
		jobs = append(jobs, func(ctx context.Context) error {
			got, err := e.containerShares(ctx, p, start, end, snap)
			if err != nil {
				return err
			}
			mu.Lock()
			shares = got
			mu.Unlock()
			return nil
		})
	}
	if err := parallel(ctx, jobs...); err != nil {
		return nil, err
	}

	for i := range rows {
		rows[i].M.normalizeSplit()
	}
	rows = attributeNetworkAndLogs(rows, net, logs, p)
	if p.containerDim >= 0 || p.containerFilter != nil {
		rows = splitContainers(rows, shares, p.containerDim, p.containerFilter)
	}
	return aggregate(aggInput{
		Dims:        q.Dims,
		Rows:        rows,
		Buckets:     clampBuckets(bws, end),
		Totals:      totals,
		NodeCost:    nodeCost,
		Shared:      p.shared,
		IncludeIdle: q.IncludeIdle,
		ShareIdle:   q.ShareIdle,
	}), nil
}

// clampBuckets makes each bucket's data end no later than now.
func clampBuckets(bws []bucketWindow, end time.Time) []bucketWindow {
	out := make([]bucketWindow, len(bws))
	for i, b := range bws {
		if b.End.After(end) {
			b.End = end
		}
		if b.End.Before(b.Start) {
			b.End = b.Start
		}
		out[i] = b
	}
	return out
}

// plan is the compiled, source-independent part of a query.
type plan struct {
	q               AllocationQuery
	forceRaw        bool
	filtered        bool // any filter beyond cluster
	clusterIDs      []string
	labelKeys       []string
	containerDim    int // index in Dims, -1 if absent
	containerFilter map[string]bool
	shared          map[string]bool
	sharedList      []string
	// netSynthOK: network cost of workloads with no compute rows may
	// get a synthetic row (only when no filter could have excluded it).
	netSynthOK bool
}

func newPlan(q AllocationQuery, snap *clusters.Snapshot) *plan {
	p := &plan{q: q, containerDim: -1, shared: map[string]bool{}, netSynthOK: true}
	for _, d := range q.Dims {
		if d.rawOnly() {
			p.forceRaw = true
		}
		if d.isLabel() {
			p.labelKeys = appendUnique(p.labelKeys, d.LabelKey)
		}
	}
	for i, d := range q.Dims {
		if d.Name == DimContainer {
			p.containerDim = i
		}
	}
	var clusterSets [][]string
	if q.ClusterID != "" {
		clusterSets = append(clusterSets, snap.Aliases(q.ClusterID))
	}
	for _, f := range q.Filters {
		switch {
		case f.Dim.Name == DimCluster:
			var al []string
			for _, v := range f.Values {
				al = append(al, snap.Aliases(v)...)
			}
			clusterSets = append(clusterSets, al)
			continue
		case f.Dim.rawOnly():
			p.forceRaw = true
		case f.Dim.isLabel():
			p.labelKeys = appendUnique(p.labelKeys, f.Dim.LabelKey)
		case f.Dim.Name == DimContainer:
			p.containerFilter = map[string]bool{}
			for _, v := range f.Values {
				p.containerFilter[v] = true
			}
		}
		p.filtered = true
		if f.Dim.Name != DimNamespace && f.Dim.Name != DimWorkload {
			p.netSynthOK = false
		}
	}
	p.clusterIDs = intersect(clusterSets)
	for _, ns := range q.SharedNamespaces {
		p.shared[ns] = true
		p.sharedList = append(p.sharedList, ns)
	}
	return p
}

// clusterCond restricts a table's cluster_id column to the requested
// clusters. An explicit filter that intersects to nothing must match
// nothing, not everything.
func (p *plan) clusterCond(w *chsql.Where, col string) {
	if p.clusterIDs == nil {
		return
	}
	if len(p.clusterIDs) == 0 {
		w.Add("0")
		return
	}
	w.In(col, p.clusterIDs)
}

func intersect(sets [][]string) []string {
	if len(sets) == 0 {
		return nil
	}
	cur := map[string]bool{}
	for _, v := range sets[0] {
		cur[v] = true
	}
	for _, s := range sets[1:] {
		next := map[string]bool{}
		for _, v := range s {
			if cur[v] {
				next[v] = true
			}
		}
		cur = next
	}
	out := []string{}
	for _, v := range sets[0] {
		if cur[v] {
			out = appendUnique(out, v)
		}
	}
	return out
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// tableSpec describes the two cost sources.
type tableSpec struct {
	table    string
	secExpr  string // unix seconds of a row
	timeCond func(w *chsql.Where, seg segment)
	metrics  string
}

var costTables = map[source]tableSpec{
	srcRollup: {
		table:   "workload_cost_1h",
		secExpr: "toInt64(toUnixTimestamp(t.ts_hour))",
		timeCond: func(w *chsql.Where, seg segment) {
			w.Add("t.ts_hour >= toDateTime(?) AND t.ts_hour < toDateTime(?)", seg.from.Unix(), seg.to.Unix())
		},
		metrics: `sum(t.cost_usd), sum(t.cpu_cost_usd), sum(t.ram_cost_usd), sum(t.gpu_cost_usd), sum(t.recoverable_usd),
			sum(t.cpu_request_core_seconds), sum(t.cpu_usage_core_seconds),
			sum(greatest(t.cpu_request_core_seconds, t.cpu_usage_core_seconds)),
			sum(t.ram_request_byte_seconds), sum(t.ram_usage_byte_seconds),
			sum(greatest(t.ram_request_byte_seconds, t.ram_usage_byte_seconds)),
			sum(t.gpu_seconds), sum(t.pod_seconds),
			min(toInt64(toUnixTimestamp(t.ts_hour))), max(toInt64(toUnixTimestamp(t.ts_hour))) + 3600`,
	},
	srcRaw: {
		table:   "pod_cost_1s",
		secExpr: "intDiv(t.ts, 1000)",
		timeCond: func(w *chsql.Where, seg segment) {
			w.Add("t.ts >= ? AND t.ts < ?", seg.from.UnixMilli(), seg.to.UnixMilli())
		},
		metrics: `sum(t.cost_usd_sec * t.interval_sec), sum(t.cpu_cost_usd_sec * t.interval_sec),
			sum(t.ram_cost_usd_sec * t.interval_sec), sum(t.gpu_cost_usd_sec * t.interval_sec),
			sum(t.recoverable_usd_sec * t.interval_sec),
			sum(t.cpu_millicores / 1000 * t.interval_sec), sum(t.cpu_usage_millicores / 1000 * t.interval_sec),
			sum(greatest(t.cpu_millicores, t.cpu_usage_millicores) / 1000 * t.interval_sec),
			sum(toFloat64(t.mem_bytes) * t.interval_sec), sum(toFloat64(t.mem_usage_bytes) * t.interval_sec),
			sum(toFloat64(greatest(t.mem_bytes, t.mem_usage_bytes)) * t.interval_sec),
			sum(toFloat64(t.gpu_count) * t.interval_sec), sum(toFloat64(t.interval_sec)),
			intDiv(min(t.ts), 1000), intDiv(max(t.ts + toInt64(t.interval_sec * 1000)), 1000)`,
	},
}

// labelJoin joins the latest labels per workload (rollup) or per pod
// (raw) from pod_metadata.
func (p *plan) labelJoin(src source) (string, []any) {
	if len(p.labelKeys) == 0 {
		return "", nil
	}
	var w chsql.Where
	p.clusterCond(&w, "cluster_id")
	if src == srcRollup {
		return `
		LEFT JOIN (
			SELECT cluster_id AS pm_cluster, namespace AS pm_ns, workload AS pm_key,
			       argMax(labels, updated_at) AS pm_labels
			FROM pod_metadata WHERE ` + w.SQL() + `
			GROUP BY pm_cluster, pm_ns, pm_key
		) AS pm ON pm.pm_cluster = t.cluster_id AND pm.pm_ns = t.namespace AND pm.pm_key = t.workload`, w.Args()
	}
	return `
		LEFT JOIN (
			SELECT cluster_id AS pm_cluster, namespace AS pm_ns, pod AS pm_key,
			       argMax(labels, updated_at) AS pm_labels
			FROM pod_metadata WHERE ` + w.SQL() + `
			GROUP BY pm_cluster, pm_ns, pm_key
		) AS pm ON pm.pm_cluster = t.cluster_id AND pm.pm_ns = t.namespace AND pm.pm_key = t.pod`, w.Args()
}

// filterWhere compiles every SQL-pushable filter.
func (p *plan) filterWhere(w *chsql.Where) {
	p.clusterCond(w, "t.cluster_id")
	for _, f := range p.q.Filters {
		switch {
		case f.Dim.Name == DimCluster || f.Dim.Name == DimContainer:
			continue
		case f.Dim.isLabel():
			if len(f.Values) == 1 {
				w.Add("pm.pm_labels[?] = ?", f.Dim.LabelKey, f.Values[0])
			} else {
				w.Add("pm.pm_labels[?] IN (?)", f.Dim.LabelKey, f.Values)
			}
		default:
			w.In(f.Dim.column(), f.Values)
		}
	}
}

// fineRows runs the main aggregation for one segment.
func (e *Engine) fineRows(ctx context.Context, p *plan, seg segment, bk bucketing, snap *clusters.Snapshot) ([]fineRow, error) {
	ts := costTables[seg.src]
	bucketExpr, bucketArgs := bk.expr(ts.secExpr)

	sel := []string{bucketExpr + " AS b", "t.cluster_id AS c", "t.namespace AS ns", "t.workload AS wl"}
	selArgs := append([]any(nil), bucketArgs...)
	group := []string{"b", "c", "ns", "wl"}
	// extra[j] = index into the scanned extra columns for dim j, -1
	// when the dim is derived from c/ns/wl or filled later.
	extra := make([]int, len(p.q.Dims))
	n := 0
	for j, d := range p.q.Dims {
		extra[j] = -1
		switch {
		case d.Name == DimCluster || d.Name == DimNamespace || d.Name == DimWorkload || d.Name == DimContainer:
			continue
		case d.isLabel():
			sel = append(sel, fmt.Sprintf("pm.pm_labels[?] AS d%d", n))
			selArgs = append(selArgs, d.LabelKey)
		default:
			sel = append(sel, fmt.Sprintf("%s AS d%d", d.column(), n))
		}
		group = append(group, fmt.Sprintf("d%d", n))
		extra[j] = n
		n++
	}

	join, joinArgs := p.labelJoin(seg.src)
	var w chsql.Where
	ts.timeCond(&w, seg)
	p.filterWhere(&w)

	query := "SELECT " + strings.Join(sel, ", ") + ",\n\t\t\t" + ts.metrics +
		"\n\t\tFROM " + ts.table + " AS t" + join +
		"\n\t\tWHERE " + w.SQL() +
		"\n\t\tGROUP BY " + strings.Join(group, ", ") +
		"\n\t\tLIMIT ?"
	args := append(append(append(selArgs, joinArgs...), w.Args()...), e.maxRows()+1)

	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rs, err := e.CH.QueryContext(qctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("allocation query: %w", err)
	}
	defer rs.Close() //nolint:errcheck

	var out []fineRow
	extraVals := make([]string, n)
	for rs.Next() {
		var (
			r       fineRow
			bucket  int64
			cluster string
		)
		dest := []any{&bucket, &cluster, &r.Namespace, &r.Workload}
		for i := range extraVals {
			dest = append(dest, &extraVals[i])
		}
		m := &r.M
		dest = append(dest, &m.Cost, &m.CPUCost, &m.RAMCost, &m.GPUCost, &m.Recoverable,
			&m.CPUReqCS, &m.CPUUseCS, &m.CPUAllocCS, &m.RAMReqBS, &m.RAMUseBS, &m.RAMAllocBS,
			&m.GPUSec, &m.PodSec, &r.Start, &r.End)
		if err := rs.Scan(dest...); err != nil {
			return nil, fmt.Errorf("allocation scan: %w", err)
		}
		r.Bucket = int(bucket)
		r.Cluster = snap.Display(cluster)
		r.Vals = make([]string, len(p.q.Dims))
		for j, d := range p.q.Dims {
			switch {
			case d.Name == DimCluster:
				r.Vals[j] = r.Cluster
			case d.Name == DimNamespace:
				r.Vals[j] = r.Namespace
			case d.Name == DimWorkload:
				r.Vals[j] = r.Workload
			case extra[j] >= 0:
				v := extraVals[extra[j]]
				if d.Name == DimController && v == "/" {
					v = "" // neither kind nor name known
				}
				r.Vals[j] = v
			}
		}
		out = append(out, r)
		if len(out) > e.maxRows() {
			return nil, ErrTooManyRows
		}
	}
	return out, rs.Err()
}

// clusterTotals reads unfiltered per-cluster compute cost, split into
// shared and non-shared namespaces (only needed when filters hide part
// of a cluster).
func (e *Engine) clusterTotals(ctx context.Context, p *plan, seg segment, bk bucketing, snap *clusters.Snapshot,
	mu *sync.Mutex, into map[clusterKey]*clusterTotals) error {
	ts := costTables[seg.src]
	bucketExpr, args := bk.expr(ts.secExpr)
	sharedExpr := "toUInt8(0)"
	if len(p.sharedList) > 0 {
		sharedExpr = "t.namespace IN (?)"
		args = append(args, p.sharedList)
	}
	costExpr := "sum(t.cost_usd)"
	if seg.src == srcRaw {
		costExpr = "sum(t.cost_usd_sec * t.interval_sec)"
	}
	var w chsql.Where
	ts.timeCond(&w, seg)
	p.clusterCond(&w, "t.cluster_id")
	query := "SELECT " + bucketExpr + " AS b, t.cluster_id AS c, " + sharedExpr + " AS sh, " + costExpr +
		" FROM " + ts.table + " AS t WHERE " + w.SQL() + " GROUP BY b, c, sh"
	return e.scan(ctx, "cluster totals", query, append(args, w.Args()...), func(rs *sql.Rows) error {
		var (
			b     int64
			c     string
			sh    uint8
			spent float64
		)
		if err := rs.Scan(&b, &c, &sh, &spent); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		k := clusterKey{int(b), snap.Display(c)}
		ct := into[k]
		if ct == nil {
			ct = &clusterTotals{}
			into[k] = ct
		}
		if sh == 1 {
			ct.Shared += spent
		} else {
			ct.NonShared += spent
		}
		return nil
	})
}

// nodeCost reads node spend per cluster (the idle denominator).
func (e *Engine) nodeCost(ctx context.Context, p *plan, seg segment, bk bucketing, snap *clusters.Snapshot,
	mu *sync.Mutex, into map[clusterKey]float64) error {
	var (
		query string
		args  []any
		w     chsql.Where
	)
	if seg.src == srcRollup {
		bucketExpr, bargs := bk.expr("toInt64(toUnixTimestamp(ts_hour))")
		w.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", seg.from.Unix(), seg.to.Unix())
		p.clusterCond(&w, "cluster_id")
		query = "SELECT " + bucketExpr + " AS b, cluster_id AS c, sum(cost_usd) FROM node_cost_1h WHERE " + w.SQL() + " GROUP BY b, c"
		args = append(bargs, w.Args()...)
	} else {
		bucketExpr, bargs := bk.expr("intDiv(ts, 1000)")
		w.Add("ts >= ? AND ts < ?", seg.from.UnixMilli(), seg.to.UnixMilli())
		p.clusterCond(&w, "cluster_id")
		query = "SELECT " + bucketExpr + " AS b, cluster_id AS c, sum(cost_usd_sec * interval_sec) FROM node_cost_1s WHERE " + w.SQL() + " GROUP BY b, c"
		args = append(bargs, w.Args()...)
	}
	return e.scan(ctx, "node cost", query, args, func(rs *sql.Rows) error {
		var (
			b     int64
			c     string
			spent float64
		)
		if err := rs.Scan(&b, &c, &spent); err != nil {
			return err
		}
		mu.Lock()
		into[clusterKey{int(b), snap.Display(c)}] += spent
		mu.Unlock()
		return nil
	})
}

// wlKey identifies a workload within a bucket.
type wlKey struct {
	Bucket                       int
	Cluster, Namespace, Workload string
}

// PreferredDirectionSQL is the flow de-duplication rule shared by every
// network query. Pod→pod traffic is observed at both ends (egress at
// the sender, ingress at the receiver): per (src, dst) pair, prefer the
// receiver's ingress rows when the destination is a pod and the
// sender's egress rows otherwise, falling back to whichever side was
// observed. It evaluates to 1 when the ingress side should be used and
// must run in a query grouped by the pair.
const PreferredDirectionSQL = "if(dst_kind = 'pod', countIf(direction = 'ingress') > 0, countIf(direction != 'ingress') = 0)"

// networkCost attributes egress + cross-zone $ to the source workload.
func (e *Engine) networkCost(ctx context.Context, p *plan, seg segment, bk bucketing, snap *clusters.Snapshot,
	mu *sync.Mutex, into map[wlKey]*metrics) error {
	var (
		table, secExpr string
		w              chsql.Where
	)
	if seg.src == srcRollup {
		table, secExpr = "net_flows_1h", "toInt64(toUnixTimestamp(ts_hour))"
		w.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", seg.from.Unix(), seg.to.Unix())
	} else {
		table, secExpr = "net_flows", "toInt64(toUnixTimestamp(ts))"
		w.Add("ts >= toDateTime(?) AND ts < toDateTime(?)", seg.from.Unix(), seg.to.Unix())
	}
	w.Add("src_kind = 'pod'")
	p.clusterCond(&w, "cluster_id")
	for _, f := range p.q.Filters {
		switch f.Dim.Name {
		case DimNamespace:
			w.In("src_namespace", f.Values)
		case DimWorkload:
			w.In("src_workload", f.Values)
		}
	}
	bucketExpr, bargs := bk.expr(secExpr)
	query := `
		SELECT b, c, ns, wl,
		       sum(if(use_in, c_in, c_eg)), sum(if(use_in, xz_in, xz_eg)), sum(if(use_in, e_in, e_eg))
		FROM (
			SELECT ` + bucketExpr + ` AS b, cluster_id AS c, src_namespace AS ns, src_workload AS wl,
			       ` + PreferredDirectionSQL + ` AS use_in,
			       sumIf(cost_usd, direction = 'ingress') AS c_in,
			       sumIf(cost_usd, direction != 'ingress') AS c_eg,
			       sumIf(cost_usd, direction = 'ingress' AND cross_zone = 1) AS xz_in,
			       sumIf(cost_usd, direction != 'ingress' AND cross_zone = 1) AS xz_eg,
			       sumIf(cost_usd, direction = 'ingress' AND egress = 1) AS e_in,
			       sumIf(cost_usd, direction != 'ingress' AND egress = 1) AS e_eg
			FROM ` + table + `
			WHERE ` + w.SQL() + `
			GROUP BY b, c, ns, wl, dst_kind, dst_namespace, dst_workload, dst_service, dst_name
		)
		GROUP BY b, c, ns, wl`
	return e.scan(ctx, "network cost", query, append(bargs, w.Args()...), func(rs *sql.Rows) error {
		var (
			b               int64
			c, ns, wl       string
			total, xz, inet float64
		)
		if err := rs.Scan(&b, &c, &ns, &wl, &total, &xz, &inet); err != nil {
			return err
		}
		if total == 0 {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		k := wlKey{int(b), snap.Display(c), ns, wl}
		m := into[k]
		if m == nil {
			m = &metrics{}
			into[k] = m
		}
		m.NetCost += total
		m.NetCrossZoneCost += xz
		m.NetInternetCost += inet
		return nil
	})
}

// logVolume attributes ingested log bytes to workloads.
func (e *Engine) logVolume(ctx context.Context, p *plan, start, end time.Time, bk bucketing, snap *clusters.Snapshot,
	mu *sync.Mutex, into map[wlKey]float64) error {
	var w chsql.Where
	w.Add("ts_minute >= toDateTime(?) AND ts_minute < toDateTime(?)", start.Unix(), end.Unix())
	p.clusterCond(&w, "cluster_id")
	for _, f := range p.q.Filters {
		switch f.Dim.Name {
		case DimNamespace, DimWorkload:
			w.In(f.Dim.Name, f.Values)
		}
	}
	bucketExpr, bargs := bk.expr("toInt64(toUnixTimestamp(ts_minute))")
	query := "SELECT " + bucketExpr + " AS b, cluster_id AS c, namespace AS ns, workload AS wl, sum(bytes)" +
		" FROM log_volume_1m WHERE " + w.SQL() + " GROUP BY b, c, ns, wl"
	return e.scan(ctx, "log volume", query, append(bargs, w.Args()...), func(rs *sql.Rows) error {
		var (
			b         int64
			c, ns, wl string
			bytes     uint64
		)
		if err := rs.Scan(&b, &c, &ns, &wl, &bytes); err != nil {
			return err
		}
		mu.Lock()
		into[wlKey{int(b), snap.Display(c), ns, wl}] += float64(bytes)
		mu.Unlock()
		return nil
	})
}

// wlID identifies a workload regardless of bucket.
type wlID struct{ Cluster, Namespace, Workload string }

type containerShare struct {
	Name     string
	CPU, RAM float64 // share of the workload's requests, each summing to 1
}

// containerShares reads each workload's per-container requests — the
// cost tables are pod-grained, so container allocation splits a pod's
// cost by the containers' share of its CPU and memory requests.
func (e *Engine) containerShares(ctx context.Context, p *plan, start, end time.Time, snap *clusters.Snapshot) (map[wlID][]containerShare, error) {
	var w chsql.Where
	w.Add("ts_5m >= toDateTime(?) AND ts_5m < toDateTime(?)", timewin.FloorHour(start).Unix(), end.Unix())
	p.clusterCond(&w, "cluster_id")
	for _, f := range p.q.Filters {
		switch f.Dim.Name {
		case DimNamespace, DimWorkload:
			w.In(f.Dim.Name, f.Values)
		}
	}
	query := "SELECT cluster_id, namespace, workload, container, avg(cpu_request), avg(mem_request)" +
		" FROM container_usage_5m WHERE " + w.SQL() +
		" GROUP BY cluster_id, namespace, workload, container LIMIT ?"
	type req struct {
		name     string
		cpu, mem float64
	}
	raw := map[wlID][]req{}
	n := 0
	err := e.scan(ctx, "container shares", query, append(w.Args(), e.maxRows()+1), func(rs *sql.Rows) error {
		var (
			c, ns, wl, name string
			cpu, mem        float64
		)
		if err := rs.Scan(&c, &ns, &wl, &name, &cpu, &mem); err != nil {
			return err
		}
		if n++; n > e.maxRows() {
			return ErrTooManyRows
		}
		k := wlID{snap.Display(c), ns, wl}
		raw[k] = append(raw[k], req{name, cpu, mem})
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make(map[wlID][]containerShare, len(raw))
	for k, reqs := range raw {
		var cpuSum, memSum float64
		for _, r := range reqs {
			cpuSum += r.cpu
			memSum += r.mem
		}
		shares := make([]containerShare, len(reqs))
		for i, r := range reqs {
			shares[i] = containerShare{Name: r.name, CPU: share(r.cpu, cpuSum, len(reqs)), RAM: share(r.mem, memSum, len(reqs))}
		}
		out[k] = shares
	}
	return out, nil
}

// share is v's fraction of sum, splitting evenly when nothing was
// requested.
func share(v, sum float64, n int) float64 {
	if sum > 0 {
		return v / sum
	}
	return 1 / float64(n)
}

// attributeNetworkAndLogs spreads each workload's network $ and log
// bytes over that workload's rows in proportion to their compute cost
// (a workload spanning nodepools/zones has several rows). Network cost
// of a workload with no compute rows gets a row of its own when no
// filter could have excluded it, so unfiltered totals stay complete.
func attributeNetworkAndLogs(rows []fineRow, net map[wlKey]*metrics, logs map[wlKey]float64, p *plan) []fineRow {
	if len(net) == 0 && len(logs) == 0 {
		return rows
	}
	idx := map[wlKey][]int{}
	for i := range rows {
		r := &rows[i]
		k := wlKey{r.Bucket, r.Cluster, r.Namespace, r.Workload}
		idx[k] = append(idx[k], i)
	}
	weights := func(ix []int) []float64 {
		var sum float64
		for _, i := range ix {
			sum += rows[i].M.Cost
		}
		out := make([]float64, len(ix))
		for j, i := range ix {
			out[j] = share(rows[i].M.Cost, sum, len(ix))
		}
		return out
	}
	for k, m := range net {
		ix := idx[k]
		if len(ix) == 0 {
			if !p.netSynthOK || p.shared[k.Namespace] {
				continue
			}
			rows = append(rows, synthRow(k, p.q.Dims, m))
			idx[k] = []int{len(rows) - 1}
			continue
		}
		for j, wgt := range weights(ix) {
			rm := &rows[ix[j]].M
			rm.NetCost += m.NetCost * wgt
			rm.NetCrossZoneCost += m.NetCrossZoneCost * wgt
			rm.NetInternetCost += m.NetInternetCost * wgt
		}
	}
	for k, b := range logs {
		ix := idx[k]
		for j, wgt := range weights(ix) {
			rows[ix[j]].M.LogBytes += b * wgt
		}
	}
	return rows
}

func synthRow(k wlKey, dims []Dim, m *metrics) fineRow {
	r := fineRow{Bucket: k.Bucket, Cluster: k.Cluster, Namespace: k.Namespace, Workload: k.Workload,
		Vals: make([]string, len(dims))}
	for j, d := range dims {
		switch d.Name {
		case DimCluster:
			r.Vals[j] = k.Cluster
		case DimNamespace:
			r.Vals[j] = k.Namespace
		case DimWorkload:
			r.Vals[j] = k.Workload
		}
	}
	r.M.NetCost = m.NetCost
	r.M.NetCrossZoneCost = m.NetCrossZoneCost
	r.M.NetInternetCost = m.NetInternetCost
	return r
}

// splitContainers replaces each row with one row per container of its
// workload, dividing CPU-side quantities by CPU-request share and
// memory-side ones by memory-request share. Everything else follows
// the resulting cost share, so the parts always sum to the whole.
func splitContainers(rows []fineRow, shares map[wlID][]containerShare, dim int, keep map[string]bool) []fineRow {
	out := make([]fineRow, 0, len(rows))
	emit := func(r fineRow, name string) {
		if dim >= 0 {
			r.Vals = append([]string(nil), r.Vals...)
			r.Vals[dim] = name
		}
		if keep != nil && !keep[name] {
			return
		}
		out = append(out, r)
	}
	for _, r := range rows {
		cs := shares[wlID{r.Cluster, r.Namespace, r.Workload}]
		if len(cs) == 0 {
			emit(r, "")
			continue
		}
		for _, c := range cs {
			part := r
			m := r.M
			pm := &part.M
			pm.CPUCost, pm.GPUCost = m.CPUCost*c.CPU, m.GPUCost*c.CPU
			pm.CPUReqCS, pm.CPUUseCS, pm.CPUAllocCS = m.CPUReqCS*c.CPU, m.CPUUseCS*c.CPU, m.CPUAllocCS*c.CPU
			pm.GPUSec = m.GPUSec * c.CPU
			pm.RAMCost = m.RAMCost * c.RAM
			pm.RAMReqBS, pm.RAMUseBS, pm.RAMAllocBS = m.RAMReqBS*c.RAM, m.RAMUseBS*c.RAM, m.RAMAllocBS*c.RAM
			pm.Cost = pm.CPUCost + pm.RAMCost + pm.GPUCost
			f := c.CPU
			if m.Cost > 0 {
				f = pm.Cost / m.Cost
			}
			pm.Recoverable = m.Recoverable * f
			pm.PodSec = m.PodSec * f
			pm.NetCost, pm.NetCrossZoneCost, pm.NetInternetCost = m.NetCost*f, m.NetCrossZoneCost*f, m.NetInternetCost*f
			pm.LogBytes = m.LogBytes * f
			emit(part, c.Name)
		}
	}
	return out
}

// scan runs a query with the engine timeout and feeds each row to fn.
func (e *Engine) scan(ctx context.Context, what, query string, args []any, fn func(*sql.Rows) error) error {
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rs, err := e.CH.QueryContext(qctx, query, args...)
	if err != nil {
		return fmt.Errorf("%s query: %w", what, err)
	}
	defer rs.Close() //nolint:errcheck
	for rs.Next() {
		if err := fn(rs); err != nil {
			if errors.Is(err, ErrTooManyRows) {
				return err
			}
			return fmt.Errorf("%s scan: %w", what, err)
		}
	}
	if err := rs.Err(); err != nil {
		return fmt.Errorf("%s rows: %w", what, err)
	}
	return nil
}

// parallel runs jobs concurrently and returns the first error,
// cancelling the rest.
func parallel(ctx context.Context, jobs ...func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg    sync.WaitGroup
		once  sync.Once
		first error
	)
	for _, j := range jobs {
		wg.Add(1)
		go func(j func(context.Context) error) {
			defer wg.Done()
			if err := j(ctx); err != nil {
				once.Do(func() {
					first = err
					cancel()
				})
			}
		}(j)
	}
	wg.Wait()
	return first
}
