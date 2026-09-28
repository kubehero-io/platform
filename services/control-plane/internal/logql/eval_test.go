// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"testing"
	"time"
)

var evalBase = time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)

// testRows: three streams over two hours with second-precision
// timestamps (so many lines sit exactly on minute boundaries), json
// bodies and a mix of levels.
func testRows(seed int64) []Row {
	r := rand.New(rand.NewSource(seed))
	streams := []map[string]string{
		{"namespace": "shop", "workload": "api", "pod": "api-1", "level": "error"},
		{"namespace": "shop", "workload": "api", "pod": "api-2", "level": "info"},
		{"namespace": "pay", "workload": "ledger", "pod": "ledger-0", "level": "info", "app": "ledger"},
	}
	var rows []Row
	for sec := 0; sec < 7200; sec++ {
		for si, st := range streams {
			if r.Intn(4) != 0 {
				continue
			}
			status := 200
			if r.Intn(5) == 0 {
				status = 500 + r.Intn(4)
			}
			ts := evalBase.Add(time.Duration(sec) * time.Second)
			if si == 2 && sec%7 == 0 {
				ts = ts.Add(time.Duration(r.Intn(1000)) * time.Millisecond) // some sub-second stamps
			}
			rows = append(rows, Row{
				TS:     ts.UnixNano(),
				Line:   fmt.Sprintf(`{"status":%d,"msg":"%s","n":%d}`, status, strings.Repeat("x", r.Intn(20)), sec),
				Labels: st,
			})
		}
	}
	SortRows(rows)
	return rows
}

// brute evaluates a range aggregation directly from its definition:
// for each step t, the lines with t-offset-range < ts <= t-offset.
func brute(rows []Row, r *RangeAggExpr, t0, dt int64, steps int) map[string][]float64 {
	out := map[string][]float64{}
	p := NewPipeline(r.Selector.Stages)
	secs := r.Range.Seconds()
	for i := 0; i < steps; i++ {
		t := t0 + int64(i)*dt - int64(r.Offset)
		counts := map[string]float64{}
		bytes := map[string]float64{}
		for _, row := range rows {
			if row.TS <= t-int64(r.Range) || row.TS > t || !MatchStream(r.Selector.Matchers, row.Labels) {
				continue
			}
			line, labels, ok := p.Process(row.TS, row.Line, row.Labels)
			if !ok {
				continue
			}
			k := labelsKey(SeriesLabels(labels, nil))
			counts[k]++
			bytes[k] += float64(len(line))
		}
		for k, c := range counts {
			if out[k] == nil {
				out[k] = make([]float64, steps)
				for j := range out[k] {
					out[k][j] = math.NaN()
				}
			}
			switch r.Op {
			case RangeCount:
				out[k][i] = c
			case RangeRate:
				out[k][i] = c / secs
			case RangeBytes:
				out[k][i] = bytes[k]
			case RangeBRate:
				out[k][i] = bytes[k] / secs
			}
		}
	}
	return out
}

func resultMap(res *Result, t0, dt int64, steps int) map[string][]float64 {
	out := map[string][]float64{}
	for _, s := range res.Series {
		vals := make([]float64, steps)
		for j := range vals {
			vals[j] = math.NaN()
		}
		for _, p := range s.Points {
			i := 0
			if dt > 0 {
				i = int((p.TS.UnixNano() - t0) / dt)
			}
			vals[i] = p.Value
		}
		out[labelsKey(s.Labels)] = vals
	}
	return out
}

func sameValues(t *testing.T, name string, got, want map[string][]float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d series, want %d", name, len(got), len(want))
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok {
			t.Fatalf("%s: missing series %q", name, k)
		}
		for i := range w {
			if math.IsNaN(w[i]) != math.IsNaN(g[i]) || !math.IsNaN(w[i]) && math.Abs(w[i]-g[i]) > 1e-9*math.Max(1, math.Abs(w[i])) {
				t.Fatalf("%s: series %q step %d = %v, want %v", name, k, i, g[i], w[i])
			}
		}
	}
}

func TestRangeAggregationsMatchBruteForce(t *testing.T) {
	rows := testRows(1)
	src := &MemSource{Rows: rows}
	queries := []string{
		`count_over_time({namespace="shop"}[5m])`,
		`rate({namespace=~"shop|pay"}[1m])`,
		`bytes_over_time({workload="api"} |= "500" [2m])`,
		`bytes_rate({namespace="pay"}[90s])`,
		`count_over_time({namespace="shop"} | json | status >= 500 [5m])`,
		`count_over_time({namespace="shop"} | json | status >= 500 [5m] offset 3m)`,
		`count_over_time({app="ledger"} | json | line_format "{{.status}}" [10m])`,
	}
	grids := []struct {
		name    string
		start   time.Time
		end     time.Time
		step    time.Duration
		instant bool
	}{
		{"1m step", evalBase.Add(10 * time.Minute), evalBase.Add(110 * time.Minute), time.Minute, false},
		{"15s step", evalBase.Add(20 * time.Minute), evalBase.Add(50 * time.Minute), 15 * time.Second, false},
		{"7s step (gcd 1s)", evalBase.Add(20 * time.Minute), evalBase.Add(40 * time.Minute), 7 * time.Second, false},
		{"7m step (step > range)", evalBase.Add(5 * time.Minute), evalBase.Add(115 * time.Minute), 7 * time.Minute, false},
		{"unaligned start", evalBase.Add(10*time.Minute + 1234567), evalBase.Add(70 * time.Minute), 30 * time.Second, false},
		{"instant", evalBase.Add(60 * time.Minute), evalBase.Add(60 * time.Minute), 0, true},
	}
	for _, q := range queries {
		expr, err := ParseSampleExpr(q)
		if err != nil {
			t.Fatal(err)
		}
		r := expr.(*RangeAggExpr)
		for _, g := range grids {
			t.Run(q+"/"+g.name, func(t *testing.T) {
				res, err := Evaluate(context.Background(), expr, g.start, g.end, g.step, src)
				if err != nil {
					t.Fatal(err)
				}
				t0, dt, steps, _ := stepGrid(g.start, g.end, g.step)
				want := brute(rows, r, t0, dt, steps)
				if len(want) == 0 {
					t.Fatal("reference produced no data; test data is wrong")
				}
				sameValues(t, q, resultMap(res, t0, dt, steps), want)
			})
		}
	}
}

// Lines exactly on a window's edges: (t-range, t] excludes the left
// edge and includes the right one.
func TestWindowEdges(t *testing.T) {
	at := func(d time.Duration) int64 { return evalBase.Add(d).UnixNano() }
	lbl := map[string]string{"app": "a"}
	src := &MemSource{Rows: []Row{
		{TS: at(0), Line: "left edge of the 10:05 window", Labels: lbl},
		{TS: at(time.Nanosecond), Line: "just inside", Labels: lbl},
		{TS: at(5 * time.Minute), Line: "right edge", Labels: lbl},
		{TS: at(5*time.Minute + time.Nanosecond), Line: "next window", Labels: lbl},
	}}
	expr, _ := ParseSampleExpr(`count_over_time({app="a"}[5m])`)
	res, err := Evaluate(context.Background(), expr, evalBase.Add(5*time.Minute), evalBase.Add(10*time.Minute), 5*time.Minute, src)
	if err != nil {
		t.Fatal(err)
	}
	pts := res.Series[0].Points
	if len(pts) != 2 || pts[0].Value != 2 || pts[1].Value != 1 {
		t.Fatalf("points = %+v, want 10:05→2 (just inside + right edge), 10:10→1", pts)
	}
}

// Rollup-style buckets with edge counters must equal raw buckets.
func TestEdgeCorrection(t *testing.T) {
	g, err := NewGrid(evalBase.Add(5*time.Minute), evalBase.Add(10*time.Minute), time.Minute, 5*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Raw (Origin, …] buckets vs rollup [minute, minute+60s) buckets for
	// lines at 10:00:00 (on the edge), 10:00:30 and 10:05:00 (on the edge).
	raw := make([]float64, g.N)
	for _, ts := range []time.Duration{0, 30 * time.Second, 5 * time.Minute} {
		if b := g.Bucket(evalBase.Add(ts).UnixNano()); b >= 0 {
			raw[b]++
		}
	}
	rollup := make([]float64, g.N)
	edge := make([]float64, g.N)
	for _, ts := range []time.Duration{0, 30 * time.Second, 5 * time.Minute} {
		minute := evalBase.Add(ts).Truncate(time.Minute).UnixNano()
		b := int((minute - g.Origin) / g.Width)
		rollup[b]++
		if evalBase.Add(ts).UnixNano() == minute {
			edge[b]++
		}
	}
	want := windowSums(g, raw, nil)
	got := windowSums(g, rollup, edge)
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("step %d: rollup+edges = %v, raw = %v", i, got[i], want[i])
		}
	}
}

// spySource records the Keep hint a range query was given.
type spySource struct {
	MemSource
	keep [][]string
}

func (s *spySource) Range(ctx context.Context, q RangeQuery) ([]BucketSeries, error) {
	s.keep = append(s.keep, q.Keep)
	return s.MemSource.Range(ctx, q)
}

func TestAggregationsAndPruning(t *testing.T) {
	rows := testRows(2)
	start, end, step := evalBase.Add(10*time.Minute), evalBase.Add(40*time.Minute), time.Minute
	raw := func(q string) *Result {
		t.Helper()
		e, err := ParseSampleExpr(q)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Evaluate(context.Background(), e, start, end, step, &MemSource{Rows: rows})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return res
	}
	perPod := raw(`count_over_time({namespace="shop"}[5m])`)
	byNS := raw(`sum by (namespace) (count_over_time({namespace="shop"}[5m]))`)
	if len(perPod.Series) != 2 || len(byNS.Series) != 1 || byNS.Series[0].Labels["namespace"] != "shop" || len(byNS.Series[0].Labels) != 1 {
		t.Fatalf("series: per pod %d, by ns %v", len(perPod.Series), byNS.Series)
	}
	for i, p := range byNS.Series[0].Points {
		want := perPod.Series[0].Points[i].Value + perPod.Series[1].Points[i].Value
		if p.Value != want {
			t.Fatalf("sum at %d = %v, want %v", i, p.Value, want)
		}
		a, b := perPod.Series[0].Points[i].Value, perPod.Series[1].Points[i].Value
		check := func(q string, want float64) {
			t.Helper()
			got := raw(q).Series[0].Points[i].Value
			if math.Abs(got-want) > 1e-9 {
				t.Fatalf("%s at %d = %v, want %v", q, i, got, want)
			}
		}
		if i == 3 {
			check(`avg by (namespace) (count_over_time({namespace="shop"}[5m]))`, (a+b)/2)
			check(`max(count_over_time({namespace="shop"}[5m]))`, math.Max(a, b))
			check(`min(count_over_time({namespace="shop"}[5m]))`, math.Min(a, b))
			check(`count(count_over_time({namespace="shop"}[5m]))`, 2)
			check(`stddev(count_over_time({namespace="shop"}[5m]))`, math.Abs(a-b)/2)
			check(`stdvar(count_over_time({namespace="shop"}[5m]))`, (a-b)*(a-b)/4)
			check(`sum(count_over_time({namespace="shop"}[5m])) * 2 + 1`, 2*(a+b)+1)
		}
	}
	top := raw(`topk(1, count_over_time({namespace=~".+"}[5m]))`)
	for _, s := range top.Series {
		for _, p := range s.Points {
			// at every step only one series may be selected
			n := 0
			for _, o := range top.Series {
				for _, q := range o.Points {
					if q.TS.Equal(p.TS) {
						n++
					}
				}
			}
			if n != 1 {
				t.Fatalf("topk(1) selected %d series at %v", n, p.TS)
			}
		}
	}
	without := raw(`sum without (pod, level) (count_over_time({namespace="shop"}[5m]))`)
	if len(without.Series) != 1 || without.Series[0].Labels["workload"] != "api" || without.Series[0].Labels["pod"] != "" {
		t.Fatalf("sum without = %v", without.Series)
	}

	spy := &spySource{MemSource: MemSource{Rows: rows}}
	e, _ := ParseSampleExpr(`sum by (namespace) (rate({namespace="shop"}[5m])) + sum(count_over_time({namespace="pay"}[5m])) + max(rate({namespace="shop"}[5m]))`)
	if _, err := Evaluate(context.Background(), e, start, end, step, spy); err != nil {
		t.Fatal(err)
	}
	if len(spy.keep) != 3 || fmt.Sprint(spy.keep[0]) != "[namespace]" || spy.keep[1] == nil || len(spy.keep[1]) != 0 || spy.keep[2] != nil {
		t.Fatalf("keep hints = %#v (sum by → [namespace], sum → [], max → nil)", spy.keep)
	}
}

func TestBinaryOperators(t *testing.T) {
	at := func(m int) int64 { return evalBase.Add(time.Duration(m) * time.Minute).UnixNano() }
	a := map[string]string{"app": "a", "env": "prod"}
	b := map[string]string{"app": "b", "env": "prod"}
	c := map[string]string{"app": "a", "env": "dev"}
	var rows []Row
	for m := 1; m <= 10; m++ {
		for i := 0; i < m; i++ { // a: m lines in minute m
			rows = append(rows, Row{TS: at(m) - int64(i), Line: "x", Labels: a})
		}
		rows = append(rows, Row{TS: at(m), Line: "y", Labels: b})
		if m%2 == 0 {
			rows = append(rows, Row{TS: at(m), Line: "z", Labels: c})
		}
	}
	SortRows(rows)
	src := &MemSource{Rows: rows}
	eval := func(q string) map[string][]float64 {
		t.Helper()
		e, err := ParseSampleExpr(q)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Evaluate(context.Background(), e, evalBase.Add(time.Minute), evalBase.Add(10*time.Minute), time.Minute, src)
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return resultMap(res, at(1), int64(time.Minute), 10)
	}
	key := labelsKey
	nan := math.NaN()

	got := eval(`count_over_time({app="a", env="prod"}[1m]) > 5`)
	sameValues(t, "filter", got, map[string][]float64{key(a): {nan, nan, nan, nan, nan, 6, 7, 8, 9, 10}})

	got = eval(`count_over_time({app="a", env="prod"}[1m]) > bool 5`)
	sameValues(t, "bool", got, map[string][]float64{key(a): {0, 0, 0, 0, 0, 1, 1, 1, 1, 1}})

	got = eval(`10 - count_over_time({app="b"}[1m])`)
	sameValues(t, "scalar-vector", got, map[string][]float64{key(b): {9, 9, 9, 9, 9, 9, 9, 9, 9, 9}})

	got = eval(`sum by (env) (count_over_time({env="prod"}[1m])) / on (env) sum by (env) (count_over_time({app="b"}[1m]))`)
	sameValues(t, "on()", got, map[string][]float64{key(map[string]string{"env": "prod"}): {2, 3, 4, 5, 6, 7, 8, 9, 10, 11}})

	got = eval(`count_over_time({app="a"}[1m]) - ignoring (env) count_over_time({app="b"}[1m])`)
	// a/prod and a/dev both lose env → both match b's app=b? No: signature
	// without env is {app} — a ≠ b, so nothing matches.
	if len(got) != 0 {
		t.Fatalf("ignoring(env) across different apps must not match: %v", got)
	}

	got = eval(`count_over_time({app="a", env="prod"}[1m]) and count_over_time({app="a", env="prod"} |= "nope" [1m])`)
	if len(got) != 0 {
		t.Fatalf("and with an empty rhs = %v", got)
	}
	got = eval(`count_over_time({env="dev"}[1m]) or vector(0)`)
	if v := got[key(c)]; v[0] == v[0] || v[1] != 1 {
		t.Fatalf("or: dev series = %v", v)
	}
	if v := got[key(map[string]string{})]; v[0] != 0 {
		t.Fatalf("or: vector(0) fills gaps = %v", v)
	}
	got = eval(`count_over_time({app="a"}[1m]) unless count_over_time({env="dev"}[1m])`)
	if v := got[key(c)]; len(v) != 0 && !math.IsNaN(v[1]) {
		t.Fatalf("unless kept a/dev where dev exists: %v", v)
	}

	e, _ := ParseSampleExpr(`vector(1)+vector(1)`)
	res, err := Evaluate(context.Background(), e, evalBase, evalBase, 0, src)
	if err != nil || len(res.Series) != 1 || res.Series[0].Points[0].Value != 2 {
		t.Fatalf("vector(1)+vector(1) = %+v %v", res, err)
	}
	e, _ = ParseSampleExpr(`2 * 3 > bool 5`)
	res, err = Evaluate(context.Background(), e, evalBase, evalBase, 0, src)
	if err != nil || !res.Scalar || res.Series[0].Points[0].Value != 1 {
		t.Fatalf("scalar bool = %+v %v", res, err)
	}
}

func TestAbsentOverTime(t *testing.T) {
	at := func(m int) int64 { return evalBase.Add(time.Duration(m) * time.Minute).UnixNano() }
	src := &MemSource{Rows: []Row{{TS: at(3), Line: "x", Labels: map[string]string{"app": "a"}}}}
	e, _ := ParseSampleExpr(`absent_over_time({app="a", env=~".*"}[1m])`)
	res, err := Evaluate(context.Background(), e, evalBase.Add(time.Minute), evalBase.Add(5*time.Minute), time.Minute, src)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Series) != 1 || res.Series[0].Labels["app"] != "a" || len(res.Series[0].Labels) != 1 {
		t.Fatalf("labels = %v", res.Series)
	}
	var mins []int
	for _, p := range res.Series[0].Points {
		mins = append(mins, int(p.TS.Sub(evalBase)/time.Minute))
	}
	if fmt.Sprint(mins) != "[1 2 4 5]" {
		t.Fatalf("absent at minutes %v, want [1 2 4 5]", mins)
	}
}

func TestMetricPipelineErrors(t *testing.T) {
	src := &MemSource{Rows: []Row{
		{TS: evalBase.UnixNano(), Line: "not json", Labels: map[string]string{"app": "a"}},
		{TS: evalBase.UnixNano(), Line: `{"x":1}`, Labels: map[string]string{"app": "a"}},
	}}
	e, _ := ParseSampleExpr(`count_over_time({app="a"} | json [1m])`)
	if _, err := Evaluate(context.Background(), e, evalBase, evalBase, 0, src); err == nil || !strings.Contains(err.Error(), "JSONParserErr") {
		t.Fatalf("want a pipeline error, got %v", err)
	}
	e, _ = ParseSampleExpr(`count_over_time({app="a"} | json | __error__="" [1m])`)
	res, err := Evaluate(context.Background(), e, evalBase, evalBase, 0, src)
	if err != nil || len(res.Series) != 1 || res.Series[0].Points[0].Value != 1 {
		t.Fatalf("with __error__ filter: %+v %v", res, err)
	}
}

func TestGridLimits(t *testing.T) {
	if _, err := NewGrid(evalBase, evalBase.Add(24*time.Hour), time.Second, time.Minute, 0); err == nil {
		t.Fatal("86 401 steps must exceed MaxSteps")
	}
	if _, err := NewGrid(evalBase, evalBase.Add(10*time.Hour), 3*time.Second+time.Millisecond, 5*time.Minute, 0); err == nil {
		t.Fatal("a 1ms gcd grid must exceed MaxBuckets")
	}
	g, err := NewGrid(evalBase, evalBase.Add(time.Hour), time.Minute, 5*time.Minute, 0)
	if err != nil || g.Width != int64(time.Minute) || g.N != 66 || g.Steps != 61 {
		t.Fatalf("grid = %+v %v", g, err)
	}
}
