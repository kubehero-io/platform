// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// Evaluation limits. Prometheus caps range queries at 11 000 points
// per series; bucket grids get more room because a gcd(step, range)
// grid can be finer than the step.
const (
	MaxSteps       = 11_000
	MaxBuckets     = 200_000
	MaxSeries      = 10_000
	maxBucketCells = 20_000_000 // series × buckets held in memory
)

// ErrTooManySeries is returned when a result would exceed MaxSeries.
var ErrTooManySeries = errors.New("logql: query returns too many series (limit 10000); aggregate with sum by (…) or narrow the selector")

// Grid is where one range aggregation is evaluated: at Steps points
// Start, Start+Step, …, each over the window (t - Offset - Range,
// t - Offset]. Data is bucketed at Width = gcd(Step, Range): bucket b
// covers (Origin + b·Width, Origin + (b+1)·Width], so window i is
// exactly buckets [i·Step/Width, (i·Step+Range)/Width). Bucket N-1 is
// an extra slot holding only the edge counters the rollup path needs.
type Grid struct {
	Start, Step, Range, Offset int64 // unix ns / ns
	Steps                      int
	Origin, Width              int64
	N                          int
}

// End is the last evaluation time.
func (g Grid) End() int64 { return g.Start + int64(g.Steps-1)*g.Step }

// DataStart / DataEnd bound the lines a range aggregation reads:
// ts ∈ (DataStart, DataEnd].
func (g Grid) DataStart() int64 { return g.Origin }
func (g Grid) DataEnd() int64   { return g.End() - g.Offset }

// Bucket returns the bucket of a line timestamp, or -1 outside the data
// range.
func (g Grid) Bucket(ts int64) int {
	if ts <= g.Origin || ts > g.DataEnd() {
		return -1
	}
	return int((ts - g.Origin - 1) / g.Width)
}

func gcd(a, b int64) int64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// stepGrid returns the evaluation times t0 + i·dt, i < steps. step 0
// (or start == end) is a single instant evaluation at end.
func stepGrid(start, end time.Time, step time.Duration) (t0, dt int64, steps int, err error) {
	s, e := start.UnixNano(), end.UnixNano()
	if e < s {
		return 0, 0, 0, errors.New("logql: end is before start")
	}
	if step <= 0 || s == e {
		return e, 0, 1, nil
	}
	n := (e-s)/int64(step) + 1
	if n > MaxSteps {
		return 0, 0, 0, fmt.Errorf("logql: %d evaluation steps exceed the limit of %d; increase the step", n, MaxSteps)
	}
	return s, int64(step), int(n), nil
}

// NewGrid builds the grid for a range aggregation. step 0 (or start ==
// end) is an instant evaluation at end.
func NewGrid(start, end time.Time, step, rng, offset time.Duration) (Grid, error) {
	if rng <= 0 {
		return Grid{}, errors.New("logql: range must be positive")
	}
	t0, dt, steps, err := stepGrid(start, end, step)
	if err != nil {
		return Grid{}, err
	}
	g := Grid{Start: t0, Step: dt, Steps: steps, Range: int64(rng), Offset: int64(offset)}
	if steps == 1 {
		g.Step = g.Range // a single window: one bucket of the range's width
	}
	g.Width = gcd(g.Step, g.Range)
	g.Origin = g.Start - g.Offset - g.Range
	n := (int64(g.Steps-1)*g.Step+g.Range)/g.Width + 1
	if n > MaxBuckets {
		return Grid{}, fmt.Errorf("logql: step %s and range %s need %d buckets (limit %d); use a step that divides the range",
			FormatDuration(time.Duration(g.Step)), FormatDuration(time.Duration(g.Range)), n, MaxBuckets)
	}
	g.N = int(n)
	return g, nil
}

// window returns the bucket span [lo, hi) of evaluation step i.
func (g Grid) window(i int) (int, int) {
	lo := int(int64(i) * g.Step / g.Width)
	return lo, lo + int(g.Range/g.Width)
}

// BucketSeries is one series' per-bucket line counts and byte sums on
// a Grid. Edge* (rollup only) hold, per bucket, the lines stamped
// exactly at the bucket's left edge; see migration 0003.
type BucketSeries struct {
	Labels    map[string]string
	Count     []float64
	Bytes     []float64
	EdgeCount []float64
	EdgeBytes []float64
}

// RangeQuery asks a RangeSource for one range aggregation's buckets.
type RangeQuery struct {
	Expr *RangeAggExpr
	Grid Grid
	// Keep, when non-nil, lists the only labels the caller needs
	// (the parent is `sum by (Keep)`): the source may sum series that
	// share those labels. nil means every label is needed.
	Keep []string
}

// RangeSource produces bucketed counts for range aggregations —
// ClickHouse (raw logs or the minute rollup) or in-memory rows.
type RangeSource interface {
	Range(ctx context.Context, q RangeQuery) ([]BucketSeries, error)
}

// Result is an evaluated metric query.
type Result struct {
	Scalar bool // a scalar expression (a single label-less series)
	Series []signals.Series
	Steps  []time.Time // evaluation times
}

// Evaluate runs a metric query over [start, end] at step (step 0 or
// start == end: an instant query at end).
func Evaluate(ctx context.Context, expr SampleExpr, start, end time.Time, step time.Duration, src RangeSource) (*Result, error) {
	ev := &evaluator{src: src, start: start, end: end, step: step}
	// Every range aggregation in the query shares these evaluation times.
	t0, dt, steps, err := stepGrid(start, end, step)
	if err != nil {
		return nil, err
	}
	ev.t0, ev.dt, ev.steps = t0, dt, steps
	m, err := ev.eval(ctx, expr, nil)
	if err != nil {
		return nil, err
	}
	res := &Result{Scalar: m.scalar}
	for i := 0; i < ev.steps; i++ {
		res.Steps = append(res.Steps, time.Unix(0, ev.t0+int64(i)*ev.dt).UTC())
	}
	for _, s := range m.series {
		out := signals.Series{Labels: s.labels}
		for i := 0; i < ev.steps; i++ {
			if s.ok[i] {
				out.Points = append(out.Points, signals.Point{TS: res.Steps[i], Value: s.vals[i]})
			}
		}
		if len(out.Points) > 0 || m.scalar {
			res.Series = append(res.Series, out)
		}
	}
	sort.Slice(res.Series, func(i, j int) bool { return labelsKey(res.Series[i].Labels) < labelsKey(res.Series[j].Labels) })
	return res, nil
}

type mseries struct {
	labels map[string]string
	vals   []float64
	ok     []bool
}

type matrix struct {
	scalar bool
	series []*mseries
}

type evaluator struct {
	src        RangeSource
	start, end time.Time
	step       time.Duration
	steps      int
	t0, dt     int64
}

func (ev *evaluator) newSeries(labels map[string]string) *mseries {
	return &mseries{labels: labels, vals: make([]float64, ev.steps), ok: make([]bool, ev.steps)}
}

func (ev *evaluator) eval(ctx context.Context, e SampleExpr, keep []string) (*matrix, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch n := e.(type) {
	case *LiteralExpr:
		s := ev.newSeries(map[string]string{})
		for i := range s.vals {
			s.vals[i], s.ok[i] = n.Value, true
		}
		return &matrix{scalar: true, series: []*mseries{s}}, nil
	case *VectorExpr:
		s := ev.newSeries(map[string]string{})
		for i := range s.vals {
			s.vals[i], s.ok[i] = n.Value, true
		}
		return &matrix{series: []*mseries{s}}, nil
	case *RangeAggExpr:
		return ev.evalRange(ctx, n, keep)
	case *VectorAggExpr:
		var childKeep []string
		if _, direct := n.Inner.(*RangeAggExpr); direct && n.Op == VecSum && (n.Grouping == nil || !n.Grouping.Without) {
			childKeep = []string{}
			if n.Grouping != nil {
				childKeep = n.Grouping.Labels
			}
		}
		in, err := ev.eval(ctx, n.Inner, childKeep)
		if err != nil {
			return nil, err
		}
		return ev.aggregate(n, in)
	case *BinOpExpr:
		l, err := ev.eval(ctx, n.LHS, nil)
		if err != nil {
			return nil, err
		}
		r, err := ev.eval(ctx, n.RHS, nil)
		if err != nil {
			return nil, err
		}
		return ev.binop(n, l, r)
	}
	return nil, fmt.Errorf("logql: cannot evaluate %T", e)
}

func (ev *evaluator) evalRange(ctx context.Context, r *RangeAggExpr, keep []string) (*matrix, error) {
	grid, err := NewGrid(ev.start, ev.end, ev.step, r.Range, r.Offset)
	if err != nil {
		return nil, err
	}
	if r.Op == RangeAbsent {
		keep = []string{}
	}
	if int64(grid.N) > maxBucketCells {
		return nil, fmt.Errorf("logql: query grid too large")
	}
	buckets, err := ev.src.Range(ctx, RangeQuery{Expr: r, Grid: grid, Keep: keep})
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		if v := b.Labels[ErrorLabel]; v != "" {
			return nil, fmt.Errorf("logql: pipeline error %q for series %s; skip failed lines with | __error__=\"\" (or | __error__!=%q)", v, labelsString(b.Labels), v)
		}
	}
	if len(buckets) > MaxSeries {
		return nil, ErrTooManySeries
	}
	secs := float64(r.Range) / 1e9
	out := &matrix{}
	if r.Op == RangeAbsent {
		labels := map[string]string{}
		for _, m := range r.Selector.Matchers {
			if m.Type == MatchEqual && m.Value != "" {
				labels[m.Name] = m.Value
			}
		}
		s := ev.newSeries(labels)
		totals := make([]float64, ev.steps)
		for _, b := range buckets {
			for i, c := range windowSums(grid, b.Count, b.EdgeCount) {
				totals[i] += c
			}
		}
		for i, total := range totals {
			if total == 0 {
				s.vals[i], s.ok[i] = 1, true
			}
		}
		out.series = append(out.series, s)
		return out, nil
	}
	for _, b := range buckets {
		s := ev.newSeries(b.Labels)
		counts := windowSums(grid, b.Count, b.EdgeCount)
		var bytes []float64
		if r.Op == RangeBytes || r.Op == RangeBRate {
			bytes = windowSums(grid, b.Bytes, b.EdgeBytes)
		}
		seen := false
		for i, c := range counts {
			if c <= 0 {
				continue
			}
			var v float64
			switch r.Op {
			case RangeCount:
				v = c
			case RangeRate:
				v = c / secs
			case RangeBytes:
				v = bytes[i]
			case RangeBRate:
				v = bytes[i] / secs
			}
			s.vals[i], s.ok[i] = v, true
			seen = true
		}
		if seen {
			out.series = append(out.series, s)
		}
	}
	return out, nil
}

// windowSums totals each step's buckets [lo, hi) with a prefix sum
// (O(1) per step), then applies the edge correction: lines exactly at
// a window's left edge are outside (t - range, t], lines exactly at its
// right edge inside.
func windowSums(g Grid, vals, edge []float64) []float64 {
	prefix := make([]float64, len(vals)+1)
	for i, v := range vals {
		prefix[i+1] = prefix[i] + v
	}
	at := func(b int) float64 { // prefix clamped to the data
		if b > len(vals) {
			b = len(vals)
		}
		return prefix[b]
	}
	out := make([]float64, g.Steps)
	for i := range out {
		lo, hi := g.window(i)
		sum := at(hi) - at(lo)
		if edge != nil {
			if hi < len(edge) {
				sum += edge[hi]
			}
			if lo < len(edge) {
				sum -= edge[lo]
			}
		}
		out[i] = sum
	}
	return out
}

// ─── vector aggregation ──────────────────────────────────────────────────

func groupLabels(l map[string]string, g *Grouping) map[string]string {
	out := map[string]string{}
	if g == nil {
		return out
	}
	if g.Without {
		drop := map[string]bool{}
		for _, n := range g.Labels {
			drop[n] = true
		}
		for k, v := range l {
			if !drop[k] {
				out[k] = v
			}
		}
		return out
	}
	for _, n := range g.Labels {
		if v, ok := l[n]; ok && v != "" {
			out[n] = v
		}
	}
	return out
}

func (ev *evaluator) aggregate(n *VectorAggExpr, in *matrix) (*matrix, error) {
	if in.scalar {
		return nil, fmt.Errorf("logql: %s needs a vector, not a scalar", n.Op)
	}
	if n.Op == VecTopk || n.Op == VecBottomk {
		return ev.topk(n, in), nil
	}
	type group struct {
		s                 *mseries
		sum, sumSq, count []float64
	}
	groups := map[string]*group{}
	var order []string
	for _, s := range in.series {
		gl := groupLabels(s.labels, n.Grouping)
		key := labelsKey(gl)
		g, ok := groups[key]
		if !ok {
			g = &group{s: ev.newSeries(gl), sum: make([]float64, ev.steps), sumSq: make([]float64, ev.steps), count: make([]float64, ev.steps)}
			groups[key] = g
			order = append(order, key)
		}
		for i := 0; i < ev.steps; i++ {
			if !s.ok[i] {
				continue
			}
			v := s.vals[i]
			switch n.Op {
			case VecMin:
				if !g.s.ok[i] || v < g.s.vals[i] {
					g.s.vals[i] = v
				}
			case VecMax:
				if !g.s.ok[i] || v > g.s.vals[i] {
					g.s.vals[i] = v
				}
			}
			g.s.ok[i] = true
			g.sum[i] += v
			g.sumSq[i] += v * v
			g.count[i]++
		}
	}
	if len(groups) > MaxSeries {
		return nil, ErrTooManySeries
	}
	out := &matrix{}
	for _, key := range order {
		g := groups[key]
		for i := 0; i < ev.steps; i++ {
			if !g.s.ok[i] {
				continue
			}
			c := g.count[i]
			switch n.Op {
			case VecSum:
				g.s.vals[i] = g.sum[i]
			case VecAvg:
				g.s.vals[i] = g.sum[i] / c
			case VecCount:
				g.s.vals[i] = c
			case VecStdvar, VecStddev:
				mean := g.sum[i] / c
				v := g.sumSq[i]/c - mean*mean
				if v < 0 {
					v = 0 // rounding
				}
				if n.Op == VecStddev {
					v = math.Sqrt(v)
				}
				g.s.vals[i] = v
			}
		}
		out.series = append(out.series, g.s)
	}
	return out, nil
}

func (ev *evaluator) topk(n *VectorAggExpr, in *matrix) *matrix {
	selected := make([][]bool, len(in.series))
	for i := range selected {
		selected[i] = make([]bool, ev.steps)
	}
	groups := map[string][]int{}
	for i, s := range in.series {
		key := labelsKey(groupLabels(s.labels, n.Grouping))
		groups[key] = append(groups[key], i)
	}
	for _, members := range groups {
		for step := 0; step < ev.steps; step++ {
			var cand []int
			for _, i := range members {
				if in.series[i].ok[step] {
					cand = append(cand, i)
				}
			}
			sort.SliceStable(cand, func(a, b int) bool {
				va, vb := in.series[cand[a]].vals[step], in.series[cand[b]].vals[step]
				if n.Op == VecTopk {
					return va > vb
				}
				return va < vb
			})
			for j := 0; j < len(cand) && j < n.Param; j++ {
				selected[cand[j]][step] = true
			}
		}
	}
	out := &matrix{}
	for i, s := range in.series {
		ns := ev.newSeries(s.labels)
		seen := false
		for step := 0; step < ev.steps; step++ {
			if selected[i][step] {
				ns.vals[step], ns.ok[step] = s.vals[step], true
				seen = true
			}
		}
		if seen {
			out.series = append(out.series, ns)
		}
	}
	return out
}

// ─── binary operators ────────────────────────────────────────────────────

func applyArith(op BinOp, a, b float64) float64 {
	switch op {
	case OpAdd:
		return a + b
	case OpSub:
		return a - b
	case OpMul:
		return a * b
	case OpDiv:
		return a / b
	case OpMod:
		return math.Mod(a, b)
	case OpPow:
		return math.Pow(a, b)
	}
	return math.NaN()
}

func applyCmp(op BinOp, a, b float64) bool {
	switch op {
	case OpEq:
		return a == b
	case OpNeq:
		return a != b
	case OpGt:
		return a > b
	case OpGte:
		return a >= b
	case OpLt:
		return a < b
	}
	return a <= b
}

func (ev *evaluator) binop(n *BinOpExpr, l, r *matrix) (*matrix, error) {
	switch {
	case l.scalar && r.scalar:
		ls, rs := l.series[0], r.series[0]
		s := ev.newSeries(map[string]string{})
		for i := 0; i < ev.steps; i++ {
			s.ok[i] = true
			if n.Op.isComparison() {
				s.vals[i] = b2f(applyCmp(n.Op, ls.vals[i], rs.vals[i]))
			} else {
				s.vals[i] = applyArith(n.Op, ls.vals[i], rs.vals[i])
			}
		}
		return &matrix{scalar: true, series: []*mseries{s}}, nil
	case l.scalar || r.scalar:
		vec, sc, scalarLeft := r, l.series[0], true
		if r.scalar {
			vec, sc, scalarLeft = l, r.series[0], false
		}
		out := &matrix{}
		for _, s := range vec.series {
			ns := ev.newSeries(s.labels)
			for i := 0; i < ev.steps; i++ {
				if !s.ok[i] {
					continue
				}
				a, b := s.vals[i], sc.vals[i]
				if scalarLeft {
					a, b = b, a
				}
				switch {
				case n.Op.isComparison() && n.ReturnBool:
					ns.vals[i], ns.ok[i] = b2f(applyCmp(n.Op, a, b)), true
				case n.Op.isComparison():
					if applyCmp(n.Op, a, b) {
						ns.vals[i], ns.ok[i] = s.vals[i], true
					}
				default:
					ns.vals[i], ns.ok[i] = applyArith(n.Op, a, b), true
				}
			}
			out.series = append(out.series, ns)
		}
		return out, nil
	}
	return ev.vectorBinop(n, l, r)
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func signature(l map[string]string, m *VectorMatching) map[string]string {
	if m == nil {
		return l
	}
	out := map[string]string{}
	if m.On {
		for _, n := range m.Labels {
			if v, ok := l[n]; ok {
				out[n] = v
			}
		}
		return out
	}
	drop := map[string]bool{}
	for _, n := range m.Labels {
		drop[n] = true
	}
	for k, v := range l {
		if !drop[k] {
			out[k] = v
		}
	}
	return out
}

func (ev *evaluator) vectorBinop(n *BinOpExpr, l, r *matrix) (*matrix, error) {
	rightBySig := map[string][]*mseries{}
	for _, s := range r.series {
		k := labelsKey(signature(s.labels, n.Matching))
		rightBySig[k] = append(rightBySig[k], s)
	}
	out := &matrix{}
	switch n.Op {
	case OpAnd, OpUnless:
		for _, s := range l.series {
			rs := rightBySig[labelsKey(signature(s.labels, n.Matching))]
			ns := ev.newSeries(s.labels)
			for i := 0; i < ev.steps; i++ {
				if !s.ok[i] {
					continue
				}
				matched := false
				for _, x := range rs {
					if x.ok[i] {
						matched = true
						break
					}
				}
				if matched == (n.Op == OpAnd) {
					ns.vals[i], ns.ok[i] = s.vals[i], true
				}
			}
			out.series = append(out.series, ns)
		}
		return out, nil
	case OpOr:
		leftSig := map[string][]*mseries{}
		for _, s := range l.series {
			k := labelsKey(signature(s.labels, n.Matching))
			leftSig[k] = append(leftSig[k], s)
			out.series = append(out.series, s)
		}
		for _, s := range r.series {
			ls := leftSig[labelsKey(signature(s.labels, n.Matching))]
			ns := ev.newSeries(s.labels)
			for i := 0; i < ev.steps; i++ {
				if !s.ok[i] {
					continue
				}
				taken := false
				for _, x := range ls {
					if x.ok[i] {
						taken = true
						break
					}
				}
				if !taken {
					ns.vals[i], ns.ok[i] = s.vals[i], true
				}
			}
			out.series = append(out.series, ns)
		}
		return out, nil
	}
	for _, s := range l.series {
		rs := rightBySig[labelsKey(signature(s.labels, n.Matching))]
		if len(rs) == 0 {
			continue
		}
		labels := s.labels
		if n.Matching != nil {
			labels = signature(s.labels, n.Matching)
		}
		ns := ev.newSeries(labels)
		for i := 0; i < ev.steps; i++ {
			if !s.ok[i] {
				continue
			}
			var match *mseries
			for _, x := range rs {
				if x.ok[i] {
					if match != nil {
						return nil, fmt.Errorf("logql: many-to-many matching for %s: several series on the right match %s; aggregate them first", n.Op, labelsString(s.labels))
					}
					match = x
				}
			}
			if match == nil {
				continue
			}
			a, b := s.vals[i], match.vals[i]
			switch {
			case n.Op.isComparison() && n.ReturnBool:
				ns.vals[i], ns.ok[i] = b2f(applyCmp(n.Op, a, b)), true
			case n.Op.isComparison():
				if applyCmp(n.Op, a, b) {
					ns.vals[i], ns.ok[i] = a, true
				}
			default:
				ns.vals[i], ns.ok[i] = applyArith(n.Op, a, b), true
			}
		}
		out.series = append(out.series, ns)
	}
	return out, nil
}

// ─── label helpers ───────────────────────────────────────────────────────

// labelsKey is a canonical, collision-free key for a label set.
func labelsKey(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(l[k])
		b.WriteByte(0)
	}
	return b.String()
}

// LabelsKey exposes the canonical key for callers grouping streams.
func LabelsKey(l map[string]string) string { return labelsKey(l) }

func labelsString(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%q", k, l[k])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
