// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
)

var t0 = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func viewer() context.Context {
	return auth.WithPrincipal(context.Background(), auth.Principal{Sub: "v", Role: auth.RoleViewer})
}

// fixedRows is a MemStore over a static, sorted slice.
func fixedRows(rows []Row) *MemStore {
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TS < rows[j].TS })
	return &MemStore{Rows: func(from, to int64) []Row {
		var out []Row
		for _, r := range rows {
			if r.TS >= from && r.TS < to {
				out = append(out, r)
			}
		}
		return out
	}}
}

func sampleRows() []Row {
	var rows []Row
	for i := 0; i < 600; i++ { // one line every 6s for an hour, per pod
		for p, pod := range []string{"api-1", "api-2"} {
			level := "info"
			if i%10 == 0 {
				level = "error"
			}
			rows = append(rows, Row{
				TS:     t0.Add(-time.Hour + time.Duration(i)*6*time.Second + time.Duration(p)*time.Millisecond).UnixNano(),
				Labels: map[string]string{"cluster": "c1", "namespace": "shop", "pod": pod, "level": level, "app": "api"},
				Body:   fmt.Sprintf(`{"i":%d,"status":%d,"msg":"request %d done"}`, i, 200+300*btoi(level == "error"), i),
			})
		}
	}
	return rows
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func newEngine(store Store) *Engine {
	return New(Options{Store: store, Now: func() time.Time { return t0 }})
}

func TestQueryLogsLimitsOrderAndFilters(t *testing.T) {
	e := newEngine(fixedRows(sampleRows()))
	svc := &Service{Engine: e}
	ctx := viewer()
	res, err := svc.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{namespace="shop"} | json | status >= 500`, Limit: 7}))
	if err != nil {
		t.Fatal(err)
	}
	lines := res.Msg.GetLines()
	if res.Msg.GetResultType() != "streams" || len(lines) != 7 {
		t.Fatalf("got %d lines (%s)", len(lines), res.Msg.GetResultType())
	}
	for i, l := range lines {
		if l.GetLabels()["status"] != "500" || l.GetLevel() != "error" {
			t.Fatalf("line %d labels %v", i, l.GetLabels())
		}
		if i > 0 && l.GetTsUnixNano() > lines[i-1].GetTsUnixNano() {
			t.Fatal("backward results must be newest first")
		}
	}
	// the newest error line is i=590 (the last multiple of 10)
	if !strings.Contains(lines[0].GetBody(), `"i":590`) {
		t.Fatalf("newest line = %s", lines[0].GetBody())
	}

	fwd, err := svc.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{pod="api-2"} |= "request 1"`, Direction: "forward", Limit: 3}))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, l := range fwd.Msg.GetLines() {
		got = append(got, l.GetBody())
	}
	if len(got) != 3 || !strings.Contains(got[0], `"i":1,`) || !strings.Contains(got[1], `"i":10,`) || !strings.Contains(got[2], `"i":11,`) {
		t.Fatalf("forward lines = %v", got)
	}

	// line_format rewrites the returned body
	lf, err := svc.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{pod="api-1"} | json | line_format "{{.msg}}"`, Limit: 1}))
	if err != nil || lf.Msg.GetLines()[0].GetBody() != "request 599 done" {
		t.Fatalf("line_format: %v %v", lf, err)
	}

	// cluster scoping
	none, err := svc.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{namespace="shop"}`, ClusterId: "other"}))
	if err != nil || len(none.Msg.GetLines()) != 0 {
		t.Fatalf("cluster scope: %v %v", none, err)
	}
}

func TestQueryLogsMetric(t *testing.T) {
	e := newEngine(fixedRows(sampleRows()))
	svc := &Service{Engine: e}
	res, err := svc.QueryLogs(viewer(), connect.NewRequest(&kuberov1.QueryLogsRequest{
		Query:       `sum by (pod) (count_over_time({namespace="shop", level="error"}[10m]))`,
		StartUnixMs: t0.Add(-30 * time.Minute).UnixMilli(), EndUnixMs: t0.UnixMilli(), StepMs: int64(10 * time.Minute / time.Millisecond),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Msg.GetResultType() != "matrix" || len(res.Msg.GetSeries()) != 2 {
		t.Fatalf("result = %v", res.Msg)
	}
	for _, s := range res.Msg.GetSeries() {
		if len(s.GetPoints()) != 4 {
			t.Fatalf("points = %v", s.GetPoints())
		}
		// One error line per minute per pod → 10 per (t-10m, t] window,
		// except api-1's last window: its lines sit exactly on minute
		// marks and the last one is at t0-1m, so (t0-10m, t0] holds 9.
		for i, p := range s.GetPoints() {
			want := 10.0
			if s.GetLabels()["pod"] == "api-1" && i == 3 {
				want = 9
			}
			if p.GetValue() != want {
				t.Fatalf("step %d value = %v, want %v (%v)", i, p.GetValue(), want, s.GetLabels())
			}
		}
	}
	series, err := e.QueryMetric(context.Background(), `count_over_time({pod="api-1"}[1m]) > 5`, t0.Add(-5*time.Minute), t0, time.Minute)
	// level is a stream label: api-1's info stream has 9 lines a minute
	// (> 5), its error stream 1 (filtered out).
	if err != nil || len(series) != 1 || series[0].Labels["level"] != "info" || len(series[0].Points) != 6 || series[0].Points[0].Value != 9 {
		t.Fatalf("QueryMetric = %+v %v", series, err)
	}
}

// codeOK marks cases that must succeed (connect has no OK code).
const codeOK connect.Code = 0

func TestErrorCodes(t *testing.T) {
	svc := &Service{Engine: newEngine(fixedRows(sampleRows()))}
	code := func(err error) connect.Code {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce.Code()
		}
		return connect.CodeUnknown
	}
	tests := []struct {
		req  *kuberov1.QueryLogsRequest
		want connect.Code
	}{
		{&kuberov1.QueryLogsRequest{Query: `{app=}`}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: ``}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: `{app="a"}`, Limit: MaxLimit + 1}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: `{app="a"}`, Direction: "sideways"}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: `{app="a"}`, StartUnixMs: 2000, EndUnixMs: 1000}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: `count_over_time({app="api"} | json [1m])`}, codeOK},
		{&kuberov1.QueryLogsRequest{Query: `count_over_time({app="api"}[1m])`, StepMs: 1}, connect.CodeInvalidArgument},
		{&kuberov1.QueryLogsRequest{Query: `{app="a"}`, ClusterId: "bad id!"}, connect.CodeInvalidArgument},
	}
	for _, tc := range tests {
		_, err := svc.QueryLogs(viewer(), connect.NewRequest(tc.req))
		if tc.want == codeOK {
			if err != nil {
				t.Errorf("%q: %v", tc.req.GetQuery(), err)
			}
			continue
		}
		if code(err) != tc.want {
			t.Errorf("%q: code %v (%v), want %v", tc.req.GetQuery(), code(err), err, tc.want)
		}
	}
	if _, err := svc.QueryLogs(context.Background(), connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{a="b"}`})); code(err) != connect.CodePermissionDenied {
		t.Fatalf("anonymous: %v", err)
	}
	off := &Service{Engine: New(Options{DemoFixturesDisabled: true})}
	if _, err := off.QueryLogs(viewer(), connect.NewRequest(&kuberov1.QueryLogsRequest{Query: `{a="b"}`})); code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("demo disabled: %v", err)
	}
	if _, err := off.Engine.QueryMetric(context.Background(), `count_over_time({a="b"}[1m])`, t0, t0, 0); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("QueryMetric without store: %v", err)
	}
}

func TestVolume(t *testing.T) {
	e := newEngine(fixedRows(sampleRows()))
	res, err := e.Volume(context.Background(), VolumeParams{Query: `{namespace="shop"}`, Start: t0.Add(-time.Hour), End: t0})
	if err != nil {
		t.Fatal(err)
	}
	if res.Step != time.Minute || len(res.Series) != 2 || res.Series[0].Labels["level"] != "info" {
		t.Fatalf("volume = step %v series %v", res.Step, res.Series)
	}
	if res.TotalLines != 1200 {
		t.Fatalf("total lines = %d", res.TotalLines)
	}
	var bytes int64
	for _, r := range sampleRows() {
		bytes += int64(len(r.Body))
	}
	if res.TotalBytes != bytes {
		t.Fatalf("total bytes = %d, want %d", res.TotalBytes, bytes)
	}
	// one hour of data → bytes × 24 per day × 30 days at $0.50/GB
	want := float64(bytes) * 24 * 30 / 1e9 * DefaultUSDPerGB
	if math.Abs(res.EstCostUSDMonth-want) > 1e-12 {
		t.Fatalf("cost = %v, want %v", res.EstCostUSDMonth, want)
	}
	errs := res.Series[1]
	if errs.Labels["level"] != "error" || len(errs.Points) != 60 || errs.Points[0].Value != 2 {
		t.Fatalf("error series = %v", errs)
	}
	// grouping by a parsed label (Go path) and "-" for no grouping
	res, err = e.Volume(context.Background(), VolumeParams{Query: `{namespace="shop"} | json`, GroupBy: "status", Start: t0.Add(-time.Hour), End: t0})
	if err != nil || len(res.Series) != 2 || res.Series[1].Labels["status"] != "500" {
		t.Fatalf("volume by status = %+v %v", res, err)
	}
	res, err = e.Volume(context.Background(), VolumeParams{GroupBy: "-", Start: t0.Add(-time.Hour), End: t0})
	if err != nil || len(res.Series) != 1 || len(res.Series[0].Labels) != 0 {
		t.Fatalf("volume ungrouped = %+v %v", res, err)
	}
}

func TestPatternsSampling(t *testing.T) {
	var rows []Row
	for i := 0; i < 4000; i++ {
		body := fmt.Sprintf("user %d logged in from 10.0.0.%d", i, i%250)
		level := "info"
		if i%4 == 0 {
			body = fmt.Sprintf("payment %d failed: timeout after %dms", i, 1000+i)
			level = "error"
		}
		rows = append(rows, Row{TS: t0.Add(-time.Hour + time.Duration(i)*900*time.Millisecond).UnixNano(),
			Labels: map[string]string{"app": "api", "level": level, "pod": fmt.Sprintf("p%d", i%7)}, Body: body})
	}
	full := newEngine(fixedRows(rows))
	res, err := full.Patterns(context.Background(), PatternParams{Query: `{app="api"}`, Start: t0.Add(-time.Hour), End: t0})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Patterns) != 2 || res.LinesAnalyzed != 4000 {
		t.Fatalf("patterns = %+v", res)
	}
	p0, p1 := res.Patterns[0], res.Patterns[1]
	if p0.Pattern != "user <_> logged in from <_>" || p0.Count != 3000 || p0.Level != "info" || math.Abs(p0.SharePct-75) > 1e-9 {
		t.Fatalf("p0 = %+v", p0)
	}
	if p1.Pattern != "payment <_> failed: timeout after <_>" || p1.Count != 1000 || p1.Level != "error" {
		t.Fatalf("p1 = %+v", p1)
	}
	var trend float64
	for _, pt := range p0.Trend {
		trend += pt.Value
	}
	if len(p0.Trend) != patternBuckets || trend != 3000 {
		t.Fatalf("trend len %d sum %v", len(p0.Trend), trend)
	}

	sampled := New(Options{Store: fixedRows(rows), Now: func() time.Time { return t0 }, PatternSample: 800})
	res, err = sampled.Patterns(context.Background(), PatternParams{Query: `{app="api"}`, Start: t0.Add(-time.Hour), End: t0})
	if err != nil {
		t.Fatal(err)
	}
	if res.LinesAnalyzed < 600 || res.LinesAnalyzed > 1000 {
		t.Fatalf("sampled %d lines, want ~800", res.LinesAnalyzed)
	}
	// scaled-up estimates land near the true totals
	if c := res.Patterns[0].Count; c < 2500 || c > 3500 {
		t.Fatalf("estimated count %d, want ~3000", c)
	}
}

func TestLabelsAndSeries(t *testing.T) {
	e := newEngine(fixedRows(sampleRows()))
	svc := &Service{Engine: e}
	names, err := svc.ListLogLabels(viewer(), connect.NewRequest(&kuberov1.ListLogLabelsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(names.Msg.GetNames()) != "[app cluster level namespace pod]" {
		t.Fatalf("names = %v", names.Msg.GetNames())
	}
	vals, err := svc.ListLogLabels(viewer(), connect.NewRequest(&kuberov1.ListLogLabelsRequest{Name: "pod", Query: `{level="error"}`}))
	if err != nil || fmt.Sprint(vals.Msg.GetValues()) != "[api-1 api-2]" {
		t.Fatalf("values = %v %v", vals, err)
	}
	if _, err := svc.ListLogLabels(viewer(), connect.NewRequest(&kuberov1.ListLogLabelsRequest{Name: "bad-name"})); err == nil {
		t.Fatal("invalid label name must fail")
	}
	series, err := e.Series(context.Background(), []string{`{pod="api-1"}`, `{pod=~"api-.*"}`}, "", time.Time{}, time.Time{})
	if err != nil || len(series) != 4 { // api-1/api-2 × info/error
		t.Fatalf("series = %v %v", series, err)
	}
}

func TestDemoMode(t *testing.T) {
	now := time.Date(2026, 9, 28, 12, 45, 0, 0, time.UTC) // inside the minute 40–49 spike
	e := New(Options{Now: func() time.Time { return now }})
	if !e.Demo() || e.Source() != "demo" {
		t.Fatal("no store should mean demo mode")
	}
	res, err := e.Query(context.Background(), QueryParams{Query: `{namespace="shop"} |= "timeout"`, Limit: 20})
	if err != nil || len(res.Lines) != 20 {
		t.Fatalf("demo query: %v %v", res, err)
	}
	for _, l := range res.Lines {
		if l.Labels["source"] != "demo" || l.Labels["cluster"] != DemoCluster {
			t.Fatalf("demo line not labelled: %v", l.Labels)
		}
	}
	again, _ := e.Query(context.Background(), QueryParams{Query: `{namespace="shop"} |= "timeout"`, Limit: 20})
	if again.Lines[0].Body != res.Lines[0].Body || again.Lines[19].TS != res.Lines[19].TS {
		t.Fatal("demo data must be deterministic")
	}
	vol, err := e.Volume(context.Background(), VolumeParams{Query: `{workload="checkout"}`, Start: now.Add(-time.Hour), End: now})
	if err != nil {
		t.Fatal(err)
	}
	var errPoints []float64
	for _, s := range vol.Series {
		if s.Labels["level"] == "error" {
			for _, p := range s.Points {
				errPoints = append(errPoints, p.Value)
			}
		}
	}
	peak, base := 0.0, math.Inf(1)
	for i, v := range errPoints {
		if i >= 55 { // spike minutes 12:40-12:45 are the last buckets
			peak = math.Max(peak, v)
		} else if i < 30 {
			base = math.Min(base, v)
		}
	}
	if peak < 50 || base > 10 {
		t.Fatalf("demo error spike not visible: peak %v base %v", peak, base)
	}
	pats, err := e.Patterns(context.Background(), PatternParams{Query: `{namespace="shop"}`})
	if err != nil || len(pats.Patterns) < 4 {
		t.Fatalf("demo patterns: %+v %v", pats, err)
	}
	m, err := e.QueryMetric(context.Background(), `sum by (namespace) (count_over_time({level="error"}[5m])) > 10`, now.Add(-10*time.Minute), now, time.Minute)
	if err != nil || len(m) == 0 {
		t.Fatalf("demo metric: %v %v", m, err)
	}
}

// tailStore lets a test add rows while a tail runs.
type tailStore struct {
	MemStore
	mu   sync.Mutex
	rows []Row
}

func newTailStore() *tailStore {
	s := &tailStore{}
	s.MemStore.Rows = func(from, to int64) []Row {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []Row
		for _, r := range s.rows {
			if r.TS >= from && r.TS < to {
				out = append(out, r)
			}
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].TS < out[j].TS })
		return out
	}
	return s
}

func (s *tailStore) add(ts time.Time, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, Row{TS: ts.UnixNano(), Labels: map[string]string{"app": "a", "pod": "p"}, Body: body})
}

func TestTailDedupeLateLinesAndSkip(t *testing.T) {
	store := newTailStore()
	var clockMu sync.Mutex
	now := t0
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}
	e := New(Options{Store: store, Now: clock, TailPoll: 10 * time.Millisecond, TailOverlap: 5 * time.Second, TailMaxPerPush: 2})
	store.add(t0.Add(-20*time.Second), "too old")
	store.add(t0.Add(-5*time.Second), "recent")
	store.add(t0.Add(-4*time.Second), "noise")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []string
	var dropped int64
	var pushes int
	done := make(chan error, 1)
	go func() {
		done <- e.Tail(ctx, TailParams{Query: `{app="a"} != "noise"`}, func(b TailBatch) error {
			mu.Lock()
			defer mu.Unlock()
			pushes++
			if len(b.Lines) > 2 {
				t.Errorf("push of %d lines exceeds TailMaxPerPush", len(b.Lines))
			}
			for _, l := range b.Lines {
				got = append(got, l.Body)
			}
			dropped += b.Dropped
			return nil
		})
	}()
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			l := len(got)
			mu.Unlock()
			if l >= n {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("timed out waiting for %d lines, have %v", n, got)
	}
	waitFor(1)
	advance(time.Second)
	store.add(t0.Add(500*time.Millisecond), "new 1")
	store.add(t0.Add(-2*time.Second), "late but within overlap")
	store.add(t0.Add(900*time.Millisecond), "new 2")
	waitFor(4)
	time.Sleep(50 * time.Millisecond) // several more polls: nothing may repeat

	// Fall far behind: the tail skips ahead and reports what it skipped.
	advance(2 * time.Minute)
	for i := 0; i < 5; i++ {
		store.add(t0.Add(time.Minute+time.Duration(i)*time.Second), "missed while lagging")
	}
	store.add(t0.Add(2*time.Minute), "after the skip")
	waitFor(5)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("tail returned %v, want nil on cancel", err)
	}
	mu.Lock()
	defer mu.Unlock()
	want := []string{"recent", "late but within overlap", "new 1", "new 2", "after the skip"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("tail lines = %q, want %q", got, want)
	}
	if dropped != 5 {
		t.Fatalf("dropped = %d, want the 5 lines skipped while lagging", dropped)
	}
}

// A burst larger than the per-poll budget inside the overlap window is
// delivered completely (no stall re-reading already-sent lines, no
// skip-ahead).
func TestTailBurstCatchUp(t *testing.T) {
	store := newTailStore()
	e := New(Options{Store: store, Now: func() time.Time { return t0 }, TailPoll: 5 * time.Millisecond,
		TailOverlap: 5 * time.Second, TailMaxPerPoll: 3})
	for i := 0; i < 10; i++ {
		store.add(t0.Add(-4*time.Second+time.Duration(i)*time.Millisecond), fmt.Sprintf("burst %d", i))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var got []string
	var dropped int64
	done := make(chan error, 1)
	go func() {
		done <- e.Tail(ctx, TailParams{Query: `{app="a"}`}, func(b TailBatch) error {
			mu.Lock()
			defer mu.Unlock()
			for _, l := range b.Lines {
				got = append(got, l.Body)
			}
			dropped += b.Dropped
			return nil
		})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= 10 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 10 || dropped != 0 {
		t.Fatalf("burst delivered %d lines (%v), dropped %d; want all 10, none dropped", len(got), got, dropped)
	}
	for i, b := range got {
		if b != fmt.Sprintf("burst %d", i) {
			t.Fatalf("out of order or duplicated: %v", got)
		}
	}
}

// Paging with tiny pages, lines sharing timestamps across page edges
// and a Go-side filter returns exactly what one big page returns.
func TestSelectLinesPaging(t *testing.T) {
	var rows []Row
	for i := 0; i < 200; i++ {
		ts := t0.Add(-time.Hour + time.Duration(i/4)*time.Second) // 4 lines per timestamp
		rows = append(rows, Row{TS: ts.UnixNano(), Labels: map[string]string{"app": "a", "pod": fmt.Sprintf("p%d", i%4)},
			Body: fmt.Sprintf(`{"i":%d,"keep":%v}`, i, i%7 == 0)})
	}
	big := New(Options{Store: fixedRows(append([]Row(nil), rows...)), Now: func() time.Time { return t0 }})
	for _, page := range []int{1, 3, 4, 5, 10} {
		small := New(Options{Store: fixedRows(append([]Row(nil), rows...)), Now: func() time.Time { return t0 }, ScanPageRows: page})
		for _, fwd := range []bool{false, true} {
			for _, q := range []string{`{app="a"} | json | keep="true"`, `{app="a"}`} {
				p := QueryParams{Query: q, Start: t0.Add(-2 * time.Hour), End: t0, Limit: 25, Forward: fwd}
				want, err := big.Query(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				got, err := small.Query(context.Background(), p)
				if err != nil {
					t.Fatal(err)
				}
				wk, gk := map[string]bool{}, map[string]bool{}
				for _, l := range want.Lines {
					wk[fmt.Sprint(l.TS, l.Body)] = true
				}
				for _, l := range got.Lines {
					k := fmt.Sprint(l.TS, l.Body)
					if gk[k] {
						t.Fatalf("page %d fwd=%v %s: duplicate line %s", page, fwd, q, k)
					}
					gk[k] = true
				}
				if len(got.Lines) != len(want.Lines) || len(wk) != len(gk) {
					t.Fatalf("page %d fwd=%v %s: %d lines, want %d", page, fwd, q, len(got.Lines), len(want.Lines))
				}
				for k := range wk {
					if !gk[k] {
						t.Fatalf("page %d fwd=%v %s: missing %s", page, fwd, q, k)
					}
				}
			}
		}
	}
}
