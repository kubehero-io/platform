// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	chgo "github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
)

const org = "default"

// Hard caps on what one query may make ClickHouse return.
const (
	maxBucketRows = 2_000_000 // (series × bucket) rows from a bucket query
	maxLabelKeys  = 500
	labelSample   = 200_000 // lines sampled to discover label names
)

// CHStore reads logs from ClickHouse.
type CHStore struct {
	Conn driver.Conn
	// DisableRollup forces raw-table metric and volume queries (tests
	// use it to compare both paths).
	DisableRollup bool
	// MaxGoRows caps lines a Go-evaluated pipeline may read.
	MaxGoRows int
	// QueryTimeout bounds each ClickHouse query (server-side too).
	QueryTimeout time.Duration
}

var _ Store = (*CHStore)(nil)

// Source implements Store.
func (s *CHStore) Source() string { return "clickhouse" }

func (s *CHStore) timeout() time.Duration {
	if s.QueryTimeout <= 0 {
		return 60 * time.Second
	}
	return s.QueryTimeout
}

func (s *CHStore) maxGoRows() int {
	if s.MaxGoRows <= 0 {
		return 1_000_000
	}
	return s.MaxGoRows
}

// progress accumulates ClickHouse's progress packets, which arrive on
// the driver's reader goroutine.
type progress struct{ rows, bytes atomic.Int64 }

func (p *progress) stats() ScanStats {
	if p == nil {
		return ScanStats{}
	}
	return ScanStats{Rows: p.rows.Load(), Bytes: p.bytes.Load()}
}

// query runs a statement with a deadline, the matching server-side
// execution limit and progress accounting. The returned done func must
// be called exactly once: it cancels the query first (so an early exit
// stops the server instead of draining the stream — clickhouse-go's
// Close reads every remaining block) and then closes the rows.
func (s *CHStore) query(ctx context.Context, q logql.Query) (driver.Rows, *progress, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout())
	pr := &progress{}
	ctx = chgo.Context(ctx,
		chgo.WithSettings(chgo.Settings{"max_execution_time": int(s.timeout().Seconds())}),
		chgo.WithProgress(func(p *chgo.Progress) {
			pr.rows.Add(int64(p.Rows))
			pr.bytes.Add(int64(p.Bytes))
		}))
	rows, err := s.Conn.Query(ctx, q.SQL, q.Args...)
	if err != nil {
		cancel()
		return nil, nil, nil, fmt.Errorf("clickhouse: %w", err)
	}
	return rows, pr, func() {
		cancel()
		_ = rows.Close()
	}, nil
}

// Scan implements Store.
func (s *CHStore) Scan(ctx context.Context, req ScanRequest, fn func(Row) bool) (ScanStats, error) {
	plan := req.Plan
	if req.Sample > 0 && req.Sample < 1 {
		plan = plan.WithSample(req.Sample)
	}
	rows, pr, done, err := s.query(ctx, plan.LogsQuery(org, req.From, req.To, req.Forward, req.Max))
	if err != nil {
		return ScanStats{}, err
	}
	defer done()
	cols := make([]string, len(logql.StreamColumns))
	dest := make([]any, 0, len(cols)+4)
	var ts int64
	var trace, body string
	var extra map[string]string
	dest = append(dest, &ts)
	for i := range cols {
		dest = append(dest, &cols[i])
	}
	dest = append(dest, &trace, &extra, &body)
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return pr.stats(), fmt.Errorf("clickhouse scan: %w", err)
		}
		if !fn(Row{TS: ts, Labels: streamLabels(cols, extra), TraceID: trace, Body: body}) {
			return pr.stats(), nil
		}
	}
	if err := rows.Err(); err != nil {
		return pr.stats(), fmt.Errorf("clickhouse: %w", err)
	}
	return pr.stats(), nil
}

// Count implements Store (the plan's SQL part only).
func (s *CHStore) Count(ctx context.Context, plan *logql.Plan, _ *logql.LogSelectorExpr, from, to int64) (int64, error) {
	rows, _, done, err := s.query(ctx, plan.CountQuery(org, from, to))
	if err != nil {
		return 0, err
	}
	defer done()
	var n uint64
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return int64(n), rows.Err()
}

// Range implements logql.RangeSource: an exact plan is aggregated in
// ClickHouse (from the minute rollup when eligible), anything else is
// streamed through the Go pipeline under MaxGoRows.
func (s *CHStore) Range(ctx context.Context, q logql.RangeQuery) ([]logql.BucketSeries, error) {
	plan := logql.PlanSelector(q.Expr.Selector)
	if !plan.Exact() {
		return s.rangeGo(ctx, q, plan)
	}
	rollup := !s.DisableRollup && plan.RollupEligible(q.Grid, q.Keep)
	var stmt logql.Query
	if rollup {
		stmt = plan.RollupBucketQuery(org, q.Grid, q.Keep, maxBucketRows+1)
	} else {
		stmt = plan.BucketQuery(org, q.Grid, q.Keep, maxBucketRows+1)
	}
	rows, _, done, err := s.query(ctx, stmt)
	if err != nil {
		return nil, err
	}
	defer done()

	nLabels := len(q.Keep)
	if q.Keep == nil {
		nLabels = len(logql.StreamColumns)
	}
	labelVals := make([]string, nLabels)
	var extra map[string]string
	var bucket int64
	var c, bs, ec, eb uint64
	dest := make([]any, 0, nLabels+6)
	for i := range labelVals {
		dest = append(dest, &labelVals[i])
	}
	if q.Keep == nil {
		dest = append(dest, &extra)
	}
	dest = append(dest, &bucket, &c, &bs)
	if rollup {
		dest = append(dest, &ec, &eb)
	}

	series := map[string]*logql.BucketSeries{}
	var order []string
	n := 0
	for rows.Next() {
		if n++; n > maxBucketRows {
			return nil, logql.Limitf("the query produces more than %d series×step cells; aggregate with sum by (…) or shorten the range", maxBucketRows)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("clickhouse scan: %w", err)
		}
		var labels map[string]string
		if q.Keep == nil {
			labels = streamLabels(labelVals, extra)
		} else {
			labels = make(map[string]string, len(q.Keep))
			for i, k := range q.Keep {
				if labelVals[i] != "" {
					labels[k] = labelVals[i]
				}
			}
		}
		key := logql.LabelsKey(labels)
		bsr, ok := series[key]
		if !ok {
			if len(series) >= logql.MaxSeries {
				return nil, logql.ErrTooManySeries
			}
			bsr = &logql.BucketSeries{Labels: labels, Count: make([]float64, q.Grid.N), Bytes: make([]float64, q.Grid.N)}
			if rollup {
				bsr.EdgeCount = make([]float64, q.Grid.N)
				bsr.EdgeBytes = make([]float64, q.Grid.N)
			}
			series[key] = bsr
			order = append(order, key)
		}
		if bucket < 0 || bucket >= int64(q.Grid.N) {
			continue
		}
		bsr.Count[bucket] += float64(c)
		bsr.Bytes[bucket] += float64(bs)
		if rollup {
			bsr.EdgeCount[bucket] += float64(ec)
			bsr.EdgeBytes[bucket] += float64(eb)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	out := make([]logql.BucketSeries, 0, len(order))
	for _, k := range order {
		out = append(out, *series[k])
	}
	return out, nil
}

func (s *CHStore) rangeGo(ctx context.Context, q logql.RangeQuery, plan *logql.Plan) ([]logql.BucketSeries, error) {
	b := logql.NewBucketer(q)
	p := logql.NewPipeline(plan.Stages)
	max := s.maxGoRows()
	n := 0
	var addErr error
	_, err := s.Scan(ctx, ScanRequest{
		Selector: q.Expr.Selector, Plan: plan,
		From: q.Grid.DataStart() + 1, To: q.Grid.DataEnd() + 1,
		Forward: true, Max: max + 1,
	}, func(r Row) bool {
		if n++; n > max {
			addErr = logql.Limitf("this query would evaluate more than %d lines in Go (parsers, templates or parsed-label filters); add line filters or narrow the selector or range", max)
			return false
		}
		line, labels, ok := p.Process(r.TS, r.Body, r.Labels)
		if !ok {
			return true
		}
		if err := b.Add(r.TS, line, labels); err != nil {
			addErr = err
			return false
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	if addErr != nil {
		return nil, addErr
	}
	return b.Result(), nil
}

// Volume implements Store.
func (s *CHStore) Volume(ctx context.Context, req VolumeRequest) ([]VolumeCell, ScanStats, error) {
	if !req.Plan.Exact() {
		return volumeGo(ctx, s, req)
	}
	minute := int64(time.Minute)
	rollup := !s.DisableRollup && req.Plan.RollupOK && (req.GroupBy == "" || logschema.IsRollupLabel(req.GroupBy)) &&
		req.From%minute == 0 && req.Step%minute == 0 && req.To%minute == 0
	rows, pr, done, err := s.query(ctx, req.Plan.VolumeQuery(org, req.From, req.To, req.Step, req.GroupBy, rollup))
	if err != nil {
		return nil, ScanStats{}, err
	}
	defer done()
	var out []VolumeCell
	for rows.Next() {
		var g string
		var b int64
		var lines, bytes uint64
		if err := rows.Scan(&g, &b, &lines, &bytes); err != nil {
			return nil, pr.stats(), fmt.Errorf("clickhouse scan: %w", err)
		}
		out = append(out, VolumeCell{Group: g, Bucket: int(b), Lines: float64(lines), Bytes: float64(bytes)})
		if len(out) > maxBucketRows {
			return nil, pr.stats(), logql.Limitf("log volume has too many groups × steps; pick a coarser step or another group_by label")
		}
	}
	return out, pr.stats(), rows.Err()
}

// volumeGo evaluates volume for pipelines ClickHouse cannot run.
func volumeGo(ctx context.Context, s Store, req VolumeRequest) ([]VolumeCell, ScanStats, error) {
	p := logql.NewPipeline(req.Plan.Stages)
	cells := map[[2]string]*VolumeCell{}
	n := 0
	max := req.MaxScanned
	if max <= 0 {
		max = 1_000_000
	}
	var limitErr error
	st, err := s.Scan(ctx, ScanRequest{Selector: req.Selector, Plan: req.Plan, From: req.From, To: req.To, Forward: true, Max: max + 1},
		func(r Row) bool {
			if n++; n > max {
				limitErr = logql.Limitf("log volume: more than %d lines need Go evaluation; narrow the query", max)
				return false
			}
			line, labels, ok := p.Process(r.TS, r.Body, r.Labels)
			if !ok {
				return true
			}
			b := int((r.TS - req.From) / req.Step)
			g := ""
			if req.GroupBy != "" {
				g = labels[req.GroupBy]
			}
			k := [2]string{g, fmt.Sprint(b)}
			c, ok := cells[k]
			if !ok {
				c = &VolumeCell{Group: g, Bucket: b}
				cells[k] = c
			}
			c.Lines++
			c.Bytes += float64(len(line))
			return true
		})
	if err != nil {
		return nil, st, err
	}
	if limitErr != nil {
		return nil, st, limitErr
	}
	out := make([]VolumeCell, 0, len(cells))
	for _, c := range cells {
		out = append(out, *c)
	}
	return out, st, nil
}

// LabelNames implements Store.
func (s *CHStore) LabelNames(ctx context.Context, _ *logql.LogSelectorExpr, plan *logql.Plan, from, to int64) ([]string, error) {
	rows, _, done, err := s.query(ctx, plan.LabelNamesQuery(org, from, to, labelSample, maxLabelKeys))
	if err != nil {
		return nil, err
	}
	defer done()
	flags := make([]uint8, len(logql.StreamColumns))
	var keys []string
	dest := make([]any, 0, len(flags)+1)
	for i := range flags {
		dest = append(dest, &flags[i])
	}
	dest = append(dest, &keys)
	var names []string
	if rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("clickhouse scan: %w", err)
		}
		for i, f := range flags {
			if f == 1 {
				names = append(names, logschema.ColumnLabels[i])
			}
		}
		names = append(names, keys...)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return dedupSorted(names), nil
}

// LabelValues implements Store.
func (s *CHStore) LabelValues(ctx context.Context, _ *logql.LogSelectorExpr, plan *logql.Plan, name string, from, to int64, limit int) ([]string, error) {
	rollup := !s.DisableRollup && plan.RollupOK && logschema.IsRollupLabel(name)
	rows, _, done, err := s.query(ctx, plan.LabelValuesQuery(org, name, from, to, limit, rollup))
	if err != nil {
		return nil, err
	}
	defer done()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("clickhouse scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func dedupSorted(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// Series implements Store.
func (s *CHStore) Series(ctx context.Context, _ *logql.LogSelectorExpr, plan *logql.Plan, from, to int64, limit int) ([]map[string]string, error) {
	rows, _, done, err := s.query(ctx, plan.SeriesQuery(org, from, to, limit))
	if err != nil {
		return nil, err
	}
	defer done()
	cols := make([]string, len(logql.StreamColumns))
	var extra map[string]string
	dest := make([]any, 0, len(cols)+1)
	for i := range cols {
		dest = append(dest, &cols[i])
	}
	dest = append(dest, &extra)
	var out []map[string]string
	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("clickhouse scan: %w", err)
		}
		out = append(out, streamLabels(cols, extra))
	}
	return out, rows.Err()
}
