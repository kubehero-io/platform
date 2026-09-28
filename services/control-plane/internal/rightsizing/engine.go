// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rightsizing

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Query selects what to recommend over.
type Query struct {
	ClusterID string // any alias; "" = fleet
	Namespace string
	Workload  string
	Window    timewin.Window
	Options
}

// Engine reads usage history and prices from ClickHouse.
type Engine struct {
	CH       *sql.DB
	Clusters *clusters.Resolver
	Timeout  time.Duration // per query, default 30s
	CacheTTL time.Duration // default 60s; negative disables
	MaxRows  int           // containers per run, default 100k

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	at   time.Time
	recs []Recommendation
}

const maxCacheEntries = 64

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
	return 100_000
}

// ErrTooManyContainers means the scope is too large for one run.
var ErrTooManyContainers = errors.New("rightsizing scope too large: filter by cluster or namespace")

// Recommend returns one recommendation per container with usage in the
// window (every direction, unsorted). Results are cached briefly: the
// overview, team spend and efficiency views all ask for the same scope.
func (e *Engine) Recommend(ctx context.Context, q Query) ([]Recommendation, error) {
	if e == nil || e.CH == nil {
		return nil, errors.New("rightsizing: ClickHouse not configured")
	}
	key := fmt.Sprintf("%s|%s|%s|%d|%d|%g|%s", q.ClusterID, q.Namespace, q.Workload,
		q.Window.Start.Unix()/60, q.Window.QueryEnd().Unix()/60, q.HeadroomPct, q.CPUPercentile)
	ttl := e.CacheTTL
	if ttl == 0 {
		ttl = time.Minute
	}
	if ttl > 0 {
		e.mu.Lock()
		if c, ok := e.cache[key]; ok && time.Since(c.at) < ttl {
			e.mu.Unlock()
			return c.recs, nil
		}
		e.mu.Unlock()
	}

	snap := e.Clusters.Snapshot(ctx)
	aliases := snap.Aliases(q.ClusterID)
	start, end := q.Window.Start, q.Window.QueryEnd()

	var (
		usage  []Usage
		ooms   map[oomKey]int
		prices map[wlKey]Price
		fleet  Price
	)
	errs := make(chan error, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		usage, err = e.usage(ctx, q, aliases, start, end, snap)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		var err error
		ooms, err = e.oomKills(ctx, q, aliases, start, end, snap)
		errs <- err
	}()
	go func() {
		defer wg.Done()
		var err error
		prices, fleet, err = e.prices(ctx, q, aliases, start, end, snap)
		errs <- err
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return nil, err
		}
	}
	if fleet.CPUCoreHour == 0 && fleet.RAMGiBHour == 0 && (q.ClusterID != "" || q.Namespace != "" || q.Workload != "") {
		// Nothing priced in the filtered scope: fall back to fleet
		// averages rather than pricing savings at $0.
		if _, f, err := e.prices(ctx, Query{Window: q.Window}, nil, start, end, snap); err == nil {
			fleet = f
		}
	}

	opts := q.Options
	opts.Window = q.Window.Covered()
	recs := make([]Recommendation, 0, len(usage))
	for _, u := range usage {
		p, ok := prices[wlKey{u.Cluster, u.Namespace, u.Workload}]
		if !ok || (p.CPUCoreHour == 0 && p.RAMGiBHour == 0) {
			replicas := p.Replicas
			p = fleet
			p.Replicas = replicas
		}
		oom := ooms[oomKey{u.Cluster, u.Namespace, u.Workload, u.Container}] +
			ooms[oomKey{u.Cluster, u.Namespace, u.Workload, ""}] // pod-level events hit every container
		recs = append(recs, Recommend(u, oom, p, opts))
	}

	if ttl > 0 {
		e.mu.Lock()
		if e.cache == nil {
			e.cache = map[string]cacheEntry{}
		}
		if len(e.cache) >= maxCacheEntries {
			var oldest string
			var oldestAt time.Time
			for k, c := range e.cache {
				if oldest == "" || c.at.Before(oldestAt) {
					oldest, oldestAt = k, c.at
				}
			}
			delete(e.cache, oldest)
		}
		e.cache[key] = cacheEntry{at: time.Now(), recs: recs}
		e.mu.Unlock()
	}
	return recs, nil
}

func (e *Engine) scope(w *chsql.Where, q Query, aliases []string) {
	if aliases != nil {
		w.In("cluster_id", aliases)
	}
	if q.Namespace != "" {
		w.Add("namespace = ?", q.Namespace)
	}
	if q.Workload != "" {
		w.Add("workload = ?", q.Workload)
	}
}

func (e *Engine) usage(ctx context.Context, q Query, aliases []string, start, end time.Time, snap *clusters.Snapshot) ([]Usage, error) {
	var w chsql.Where
	w.Add("ts_5m >= toDateTime(?) AND ts_5m < toDateTime(?)", start.Unix(), end.Unix())
	e.scope(&w, q, aliases)
	query := `
		SELECT cluster_id, namespace, workload, argMax(workload_kind, ts_5m), container,
		       quantilesTDigestMerge(0.5, 0.9, 0.95, 0.99)(cpu_digest),
		       quantilesTDigestMerge(0.5, 0.9, 0.95, 0.99)(mem_digest),
		       max(cpu_max), max(mem_max),
		       argMax(cpu_request, ts_5m), argMax(cpu_limit, ts_5m),
		       argMax(mem_request, ts_5m), argMax(mem_limit, ts_5m),
		       sum(samples), uniqExact(ts_5m),
		       countIf(cpu_limit > 0 AND arrayElement(finalizeAggregation(cpu_digest), 4) >= 0.9 * cpu_limit),
		       countIf(cpu_limit > 0),
		       max(max_restarts)
		FROM container_usage_5m
		WHERE ` + w.SQL() + `
		GROUP BY cluster_id, namespace, workload, container
		LIMIT ?`
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, query, append(w.Args(), e.maxRows()+1)...)
	if err != nil {
		return nil, fmt.Errorf("rightsizing usage query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []Usage
	for rows.Next() {
		var (
			u                         Usage
			cluster                   string
			cpuQ, memQ                []float32
			cpuMax                    float32
			memMax                    uint64
			cpuReq, cpuLim            float32
			memReq, memLim            uint64
			samples, buckets          uint64
			throttled, limitedBuckets uint64
		)
		if err := rows.Scan(&cluster, &u.Namespace, &u.Workload, &u.WorkloadKind, &u.Container,
			&cpuQ, &memQ, &cpuMax, &memMax, &cpuReq, &cpuLim, &memReq, &memLim,
			&samples, &buckets, &throttled, &limitedBuckets, &u.MaxRestarts); err != nil {
			return nil, fmt.Errorf("rightsizing usage scan: %w", err)
		}
		u.Cluster = snap.Display(cluster)
		u.CPU = quantiles(cpuQ, float64(cpuMax))
		u.Mem = quantiles(memQ, float64(memMax))
		u.CPURequest, u.CPULimit = float64(cpuReq), float64(cpuLim)
		u.MemRequest, u.MemLimit = float64(memReq), float64(memLim)
		u.Samples, u.Buckets = int64(samples), int64(buckets)
		if limitedBuckets > 0 {
			u.ThrottleRisk = float64(throttled) / float64(limitedBuckets)
		}
		out = append(out, u)
		if len(out) > e.maxRows() {
			return nil, ErrTooManyContainers
		}
	}
	return out, rows.Err()
}

func quantiles(q []float32, max float64) Quantiles {
	get := func(i int) float64 {
		if i < len(q) && !math.IsNaN(float64(q[i])) {
			return float64(q[i])
		}
		return 0
	}
	out := Quantiles{P50: get(0), P90: get(1), P95: get(2), P99: get(3), Max: max}
	// A t-digest estimate can overshoot the true maximum slightly.
	for _, p := range []*float64{&out.P50, &out.P90, &out.P95, &out.P99} {
		if *p > max && max > 0 {
			*p = max
		}
	}
	return out
}

type oomKey struct{ cluster, namespace, workload, container string }

func (e *Engine) oomKills(ctx context.Context, q Query, aliases []string, start, end time.Time, snap *clusters.Snapshot) (map[oomKey]int, error) {
	var w chsql.Where
	w.Add("kind = 'oom_killed'")
	w.Add("ts >= fromUnixTimestamp64Milli(toInt64(?)) AND ts < fromUnixTimestamp64Milli(toInt64(?))", start.UnixMilli(), end.UnixMilli())
	e.scope(&w, q, aliases)
	query := `SELECT cluster_id, namespace, workload, container, sum(greatest(count, 1))
		FROM cluster_events WHERE ` + w.SQL() + ` GROUP BY cluster_id, namespace, workload, container LIMIT 100000`
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, query, w.Args()...)
	if err != nil {
		return nil, fmt.Errorf("rightsizing oom query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[oomKey]int{}
	for rows.Next() {
		var (
			k     oomKey
			c     string
			count uint64
		)
		if err := rows.Scan(&c, &k.namespace, &k.workload, &k.container, &count); err != nil {
			return nil, fmt.Errorf("rightsizing oom scan: %w", err)
		}
		k.cluster = snap.Display(c)
		out[k] += int(count)
	}
	return out, rows.Err()
}

type wlKey struct{ cluster, namespace, workload string }

// prices derives each workload's $/core-hour and $/GiB-hour from what
// it was actually billed (billing basis max(request, usage), so the
// denominator is the billed core-/byte-seconds), plus average running
// replicas; the second result is the scope-wide average used when a
// workload has no cost rows.
func (e *Engine) prices(ctx context.Context, q Query, aliases []string, start, end time.Time, snap *clusters.Snapshot) (map[wlKey]Price, Price, error) {
	var w chsql.Where
	w.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", timewin.FloorHour(start).Unix(), timewin.CeilHour(end).Unix())
	e.scope(&w, q, aliases)
	query := `
		SELECT cluster_id, namespace, workload, any(cloud),
		       sum(cpu_cost_usd), sum(ram_cost_usd),
		       sum(greatest(cpu_request_core_seconds, cpu_usage_core_seconds)),
		       sum(greatest(ram_request_byte_seconds, ram_usage_byte_seconds)),
		       sum(pod_seconds),
		       min(toInt64(toUnixTimestamp(ts_hour))), max(toInt64(toUnixTimestamp(ts_hour)))
		FROM workload_cost_1h
		WHERE ` + w.SQL() + `
		GROUP BY cluster_id, namespace, workload
		LIMIT ?`
	qctx, cancel := context.WithTimeout(ctx, e.timeout())
	defer cancel()
	rows, err := e.CH.QueryContext(qctx, query, append(w.Args(), e.maxRows()+1)...)
	if err != nil {
		return nil, Price{}, fmt.Errorf("rightsizing price query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	out := map[wlKey]Price{}
	var totCPU, totRAM, totCoreS, totByteS float64
	for rows.Next() {
		var (
			c, ns, wl, cloud   string
			cpuCost, ramCost   float64
			coreS, byteS, podS float64
			first, last        int64
		)
		if err := rows.Scan(&c, &ns, &wl, &cloud, &cpuCost, &ramCost, &coreS, &byteS, &podS, &first, &last); err != nil {
			return nil, Price{}, fmt.Errorf("rightsizing price scan: %w", err)
		}
		p := Price{Cloud: cloud}
		if coreS > 0 {
			p.CPUCoreHour = cpuCost / (coreS / 3600)
		}
		if byteS > 0 {
			p.RAMGiBHour = ramCost / (byteS / 3600 / GiB)
		}
		// Average concurrently running pods over the span the workload
		// existed in the window (hourly buckets, clamped to the window).
		spanStart := math.Max(float64(first), float64(start.Unix()))
		spanEnd := math.Min(float64(last+3600), float64(end.Unix()))
		if span := spanEnd - spanStart; span > 0 {
			p.Replicas = podS / span
		}
		out[wlKey{snap.Display(c), ns, wl}] = p
		totCPU += cpuCost
		totRAM += ramCost
		totCoreS += coreS
		totByteS += byteS
	}
	if err := rows.Err(); err != nil {
		return nil, Price{}, err
	}
	var fleet Price
	if totCoreS > 0 {
		fleet.CPUCoreHour = totCPU / (totCoreS / 3600)
	}
	if totByteS > 0 {
		fleet.RAMGiBHour = totRAM / (totByteS / 3600 / GiB)
	}
	return out, fleet, nil
}

// Filter applies the list semantics of ListRightsizing: drop "ok"
// rows, keep savings ≥ minSavings when set, sort by savings desc.
func Filter(recs []Recommendation, minSavings float64, limit int) ([]Recommendation, float64) {
	out := make([]Recommendation, 0, len(recs))
	for _, r := range recs {
		if r.Direction == OK {
			continue
		}
		if minSavings > 0 && r.SavingsMonth < minSavings {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].SavingsMonth != out[j].SavingsMonth {
			return out[i].SavingsMonth > out[j].SavingsMonth
		}
		return out[i].ID < out[j].ID
	})
	var total float64
	for _, r := range out {
		if r.SavingsMonth > 0 {
			total += r.SavingsMonth
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, total
}

// PositiveSavingsByWorkload sums each workload's recoverable $/mo.
func PositiveSavingsByWorkload(recs []Recommendation) map[[3]string]float64 {
	out := map[[3]string]float64{}
	for _, r := range recs {
		if r.SavingsMonth > 0 {
			out[[3]string{r.Cluster, r.Namespace, r.Workload}] += r.SavingsMonth
		}
	}
	return out
}
