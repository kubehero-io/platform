// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"hash/fnv"
	"sort"
	"strconv"

	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
)

// MemStore serves rows from memory with the Go pipeline standing in
// for SQL: demo mode, and a reference implementation for tests.
// Rows(from, to) must return rows with ts in [from, to), oldest first.
type MemStore struct {
	Rows   func(from, to int64) []Row
	Origin string // Source() value
}

var _ Store = (*MemStore)(nil)

// Source implements Store.
func (m *MemStore) Source() string {
	if m.Origin == "" {
		return "memory"
	}
	return m.Origin
}

// sqlPart evaluates what the plan's SQL part would: the matchers and
// the pushed-down leading stages.
func sqlPart(sel *logql.LogSelectorExpr, plan *logql.Plan) (*logql.Pipeline, []*logql.LabelMatcher) {
	pushed := sel.Stages[:len(sel.Stages)-len(plan.Stages)]
	return logql.NewPipeline(pushed), sel.Matchers
}

func sampled(r Row, rate float64) bool {
	if rate <= 0 || rate >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(strconv.FormatInt(r.TS, 10)))
	_, _ = h.Write([]byte(r.Labels["pod"]))
	return float64(h.Sum64()%1_000_000) < rate*1_000_000
}

// Scan implements Store.
func (m *MemStore) Scan(ctx context.Context, req ScanRequest, fn func(Row) bool) (ScanStats, error) {
	var st ScanStats
	rows := m.Rows(req.From, req.To)
	p, ms := sqlPart(req.Selector, req.Plan)
	n := 0
	visit := func(r Row) bool {
		st.Rows++
		st.Bytes += int64(len(r.Body))
		if !logql.MatchStream(ms, r.Labels) || !sampled(r, req.Sample) {
			return true
		}
		if _, _, ok := p.Process(r.TS, r.Body, r.Labels); !ok {
			return true
		}
		if n++; n > req.Max {
			return false
		}
		return fn(r)
	}
	if req.Forward {
		for i := range rows {
			if i%4096 == 0 && ctx.Err() != nil {
				return st, ctx.Err()
			}
			if !visit(rows[i]) {
				break
			}
		}
	} else {
		for i := len(rows) - 1; i >= 0; i-- {
			if i%4096 == 0 && ctx.Err() != nil {
				return st, ctx.Err()
			}
			if !visit(rows[i]) {
				break
			}
		}
	}
	return st, nil
}

// Count implements Store.
func (m *MemStore) Count(ctx context.Context, plan *logql.Plan, sel *logql.LogSelectorExpr, from, to int64) (int64, error) {
	var n int64
	_, err := m.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true, Max: int(^uint(0) >> 1)}, func(Row) bool {
		n++
		return true
	})
	return n, err
}

// Range implements logql.RangeSource.
func (m *MemStore) Range(ctx context.Context, q logql.RangeQuery) ([]logql.BucketSeries, error) {
	rows := m.Rows(q.Grid.DataStart()+1, q.Grid.DataEnd()+1)
	src := &logql.MemSource{Rows: make([]logql.Row, len(rows))}
	for i, r := range rows {
		src.Rows[i] = logql.Row{TS: r.TS, Line: r.Body, Labels: r.Labels}
	}
	return src.Range(ctx, q)
}

// Volume implements Store.
func (m *MemStore) Volume(ctx context.Context, req VolumeRequest) ([]VolumeCell, ScanStats, error) {
	// The whole selector pipeline runs in Go here; reuse the Go path
	// with every stage (the SQL part is applied by Scan).
	return volumeGo(ctx, m, req)
}

// LabelNames implements Store.
func (m *MemStore) LabelNames(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, from, to int64) ([]string, error) {
	seen := map[string]bool{}
	_, err := m.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true, Max: labelSample}, func(r Row) bool {
		for k := range r.Labels {
			seen[k] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, err
}

// LabelValues implements Store.
func (m *MemStore) LabelValues(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, name string, from, to int64, limit int) ([]string, error) {
	seen := map[string]bool{}
	_, err := m.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true, Max: int(^uint(0) >> 1)}, func(r Row) bool {
		if v := r.Labels[name]; v != "" {
			seen[v] = true
		}
		return true
	})
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

// Series implements Store.
func (m *MemStore) Series(ctx context.Context, sel *logql.LogSelectorExpr, plan *logql.Plan, from, to int64, limit int) ([]map[string]string, error) {
	seen := map[string]bool{}
	var out []map[string]string
	_, err := m.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true, Max: int(^uint(0) >> 1)}, func(r Row) bool {
		k := logql.LabelsKey(r.Labels)
		if !seen[k] {
			seen[k] = true
			out = append(out, r.Labels)
		}
		return len(out) < limit
	})
	return out, err
}
