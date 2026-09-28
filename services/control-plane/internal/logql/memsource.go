// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"context"
	"sort"
)

// Bucketer accumulates lines that already went through a pipeline
// into BucketSeries on a grid — the Go path for range aggregations
// whose pipeline SQL cannot evaluate, and the whole engine in demo
// mode.
type Bucketer struct {
	grid   Grid
	keep   []string // nil = keep every label
	series map[string]*BucketSeries
	order  []string
}

// NewBucketer prepares a bucketer for one range query.
func NewBucketer(q RangeQuery) *Bucketer {
	return &Bucketer{grid: q.Grid, keep: q.Keep, series: map[string]*BucketSeries{}}
}

// Add counts one processed line. It fails on a pipeline error label —
// Loki refuses metric queries over failed lines unless the query
// filters them (| __error__="") — and when the series limit is hit.
func (b *Bucketer) Add(ts int64, line string, labels map[string]string) error {
	if v := labels[ErrorLabel]; v != "" {
		return Invalidf("pipeline error %q for series %s; skip failed lines with | __error__=\"\" (or | __error__!=%q)", v, labelsString(labels), v)
	}
	bi := b.grid.Bucket(ts)
	if bi < 0 {
		return nil
	}
	labels = SeriesLabels(labels, b.keep)
	key := labelsKey(labels)
	s, ok := b.series[key]
	if !ok {
		if len(b.series) >= MaxSeries {
			return ErrTooManySeries
		}
		s = &BucketSeries{Labels: labels, Count: make([]float64, b.grid.N), Bytes: make([]float64, b.grid.N)}
		b.series[key] = s
		b.order = append(b.order, key)
	}
	s.Count[bi]++
	s.Bytes[bi] += float64(len(line))
	return nil
}

// SeriesLabels returns a fresh copy of a series' identity: empty
// values are dropped (an empty label is an absent label, as in
// Prometheus), and with keep != nil only those labels remain.
func SeriesLabels(labels map[string]string, keep []string) map[string]string {
	if keep != nil {
		out := make(map[string]string, len(keep))
		for _, k := range keep {
			if v := labels[k]; v != "" {
				out[k] = v
			}
		}
		return out
	}
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// Result returns the series in first-seen order.
func (b *Bucketer) Result() []BucketSeries {
	out := make([]BucketSeries, 0, len(b.order))
	for _, k := range b.order {
		out = append(out, *b.series[k])
	}
	return out
}

// Row is one stored line: its timestamp (unix ns), body and stream
// labels (non-empty values only).
type Row struct {
	TS     int64
	Line   string
	Labels map[string]string
}

// MatchStream reports whether stream labels satisfy every matcher.
func MatchStream(ms []*LabelMatcher, labels map[string]string) bool {
	for _, m := range ms {
		if !m.Matches(labels[m.Name]) {
			return false
		}
	}
	return true
}

// MemSource evaluates range aggregations over in-memory rows with the
// Go pipeline: demo mode, and the brute-force reference the ClickHouse
// paths are tested against.
type MemSource struct {
	Rows []Row
}

// Range implements RangeSource.
func (m *MemSource) Range(ctx context.Context, q RangeQuery) ([]BucketSeries, error) {
	b := NewBucketer(q)
	p := NewPipeline(q.Expr.Selector.Stages)
	lo, hi := q.Grid.DataStart(), q.Grid.DataEnd()
	for i, r := range m.Rows {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if r.TS <= lo || r.TS > hi || !MatchStream(q.Expr.Selector.Matchers, r.Labels) {
			continue
		}
		line, labels, ok := p.Process(r.TS, r.Line, r.Labels)
		if !ok {
			continue
		}
		if err := b.Add(r.TS, line, labels); err != nil {
			return nil, err
		}
	}
	return b.Result(), nil
}

// SortRows orders rows by timestamp (stable for equal timestamps).
func SortRows(rows []Row) {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS < rows[j].TS })
}
