// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

//go:build integration

package logs

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/chtest"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

type fixture struct {
	db     *chtest.DB
	now    time.Time
	ch     *Engine // ClickHouse, rollup enabled
	raw    *Engine // ClickHouse, rollup disabled
	paged  *Engine // ClickHouse with tiny keyset pages (ties cross page edges)
	mem    *Engine // brute-force reference over the same stored rows
	stored []Row
}

// ingestFixture writes ~2h of synthetic logs through the real
// IngestLogs handler, then loads them back as the reference data set.
func ingestFixture(t *testing.T) *fixture {
	t.Helper()
	db := chtest.Open(t, "kh_ingest_logs")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	base := now.Add(-2 * time.Hour)

	tel := telemetry.New(telemetry.Options{Writer: &clickhouse.SignalWriter{Conn: db.Native}})
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	tel.Run(runCtx)
	member := auth.WithPrincipal(ctx, auth.Principal{Sub: "collector", Role: auth.RoleMember})

	r := rand.New(rand.NewSource(7))
	var entries []*kuberov1.LogEntry
	for sec := 0; sec < 7200; sec++ {
		ts := base.Add(time.Duration(sec) * time.Second)
		if sec%11 == 0 {
			ts = ts.Add(time.Duration(r.Intn(1000)) * time.Millisecond)
		}
		pod := fmt.Sprintf("api-%d", 1+sec%2)
		status := 200
		level := "info"
		switch x := r.Intn(20); {
		case x == 0:
			status, level = 503, "error"
		case x == 1:
			status, level = 404, "warn"
		}
		body := fmt.Sprintf("method=GET path=/items/%d status=%d user=u%d dur=%dms", r.Intn(50), status, r.Intn(10), 5+r.Intn(300))
		if status == 503 && r.Intn(2) == 0 {
			body += ` err="upstream timeout"`
		}
		entries = append(entries, &kuberov1.LogEntry{TsUnixNano: ts.UnixNano(), Level: level, Stream: "stdout", Body: body,
			Source: &kuberov1.PodRef{Namespace: "shop", Workload: "api", WorkloadKind: "Deployment", Pod: pod, Container: "app", Node: "n1", Team: "web"},
			Labels: map[string]string{"app": "api"}})
		if sec%5 == 0 {
			entries = append(entries, &kuberov1.LogEntry{TsUnixNano: ts.UnixNano(), Level: "info", Body: "GET /healthz 200",
				Source: &kuberov1.PodRef{Namespace: "shop", Workload: "health", Pod: "health-0", Container: "probe"}})
		}
		if sec%3 == 0 {
			amount := r.Intn(300)
			msg, lvl := "charge created", "info"
			if r.Intn(10) == 0 {
				msg, lvl = "charge failed", "error"
			}
			body := fmt.Sprintf(`{"msg":%q,"amount":%d,"trace":"t%d"}`, msg, amount, sec)
			if sec%97 == 0 {
				body = "panic: not json"
			}
			entries = append(entries, &kuberov1.LogEntry{TsUnixNano: ts.UnixNano(), Level: lvl, Body: body,
				Source: &kuberov1.PodRef{Namespace: "pay", Workload: "ledger", Pod: "ledger-0", Container: "app", Team: "payments"},
				Labels: map[string]string{"app": "ledger"}})
		}
	}
	for i := 0; i < len(entries); i += 5000 {
		chunk := entries[i:min(i+5000, len(entries))]
		res, err := tel.IngestLogs(member, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: "c1", Entries: chunk}))
		if err != nil || int(res.Msg.GetAccepted()) != len(chunk) {
			t.Fatalf("ingest: %v %v", res, err)
		}
	}
	if err := tel.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := db.Count(t, `SELECT count() FROM logs`); n != uint64(len(entries)) {
		t.Fatalf("stored %d rows, ingested %d", n, len(entries))
	}

	// Reference data = exactly what ClickHouse stored.
	var stored []Row
	rows, err := db.Native.Query(ctx, `SELECT toUnixTimestamp64Nano(ts), `+strings.Join(logql.StreamColumns, ", ")+`, trace_id, labels, body FROM logs ORDER BY ts`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		cols := make([]string, len(logql.StreamColumns))
		var ts int64
		var trace, body string
		var extra map[string]string
		dest := []any{&ts}
		for i := range cols {
			dest = append(dest, &cols[i])
		}
		dest = append(dest, &trace, &extra, &body)
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, Row{TS: ts, Labels: streamLabels(cols, extra), TraceID: trace, Body: body})
	}
	_ = rows.Close()
	clock := func() time.Time { return now }
	return &fixture{
		db: db, now: now, stored: stored,
		ch:  New(Options{Store: &CHStore{Conn: db.Native}, Now: clock}),
		raw: New(Options{Store: &CHStore{Conn: db.Native, DisableRollup: true}, Now: clock}),
		// 7-row pages: ClickHouse returns tied timestamps in arbitrary
		// order, so page edges land inside tie groups all the time.
		paged: New(Options{Store: &CHStore{Conn: db.Native}, Now: clock, ScanPageRows: 7}),
		mem:   New(Options{Store: fixedRows(append([]Row(nil), stored...)), Now: clock}),
	}
}

func lineKey(l Line) string {
	return fmt.Sprintf("%d|%s|%s", l.TS, l.Body, logql.LabelsKey(l.Labels))
}

func sameSeries(t *testing.T, name string, got, want []signals.Series) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d series, want %d\n got %v\nwant %v", name, len(got), len(want), labelSets(got), labelSets(want))
	}
	idx := map[string]signals.Series{}
	for _, s := range got {
		idx[logql.LabelsKey(s.Labels)] = s
	}
	for _, w := range want {
		g, ok := idx[logql.LabelsKey(w.Labels)]
		if !ok {
			t.Fatalf("%s: missing series %v", name, w.Labels)
		}
		if len(g.Points) != len(w.Points) {
			t.Fatalf("%s %v: %d points, want %d", name, w.Labels, len(g.Points), len(w.Points))
		}
		for i := range w.Points {
			if !g.Points[i].TS.Equal(w.Points[i].TS) || math.Abs(g.Points[i].Value-w.Points[i].Value) > 1e-9*math.Max(1, math.Abs(w.Points[i].Value)) {
				t.Fatalf("%s %v point %d = %v@%v, want %v@%v", name, w.Labels, i, g.Points[i].Value, g.Points[i].TS, w.Points[i].Value, w.Points[i].TS)
			}
		}
	}
}

func labelSets(ss []signals.Series) []string {
	var out []string
	for _, s := range ss {
		out = append(out, fmt.Sprint(s.Labels))
	}
	sort.Strings(out)
	return out
}

func TestEngineAgainstClickHouse(t *testing.T) {
	f := ingestFixture(t)
	ctx := context.Background()

	t.Run("log queries match the reference", func(t *testing.T) {
		queries := []string{
			`{namespace="shop"}`,
			`{namespace=~"shop|pay", level!="info"}`,
			`{app="api"} |= "timeout"`,
			`{namespace="shop"} != "healthz" |~ "status=5\\d\\d"`,
			`{namespace="shop"} | logfmt | status >= 500`,
			`{namespace="shop"} | logfmt | dur > 250ms and user="u3"`,
			`{namespace="pay"} | json | amount > 250 | line_format "{{.msg}} {{.amount}}"`,
			`{namespace="pay"} | json | msg="charge failed"`,
			`{namespace="pay"} | json | __error__!=""`,
			`{pod=~"api-.*"} | regexp "user=(?P<u>\\w+)" | u="u7"`,
			`{namespace="shop", workload="api"} | pattern "method=<m> path=<p> status=<s> <_>" | s="404"`,
			`{namespace="shop"} | logfmt | drop status, dur | keep level, namespace, user`,
			`{cluster="c1", team="payments"} |= "created" or "failed"`,
		}
		// Tiny pages cost a round trip per 7 rows, so they run on a
		// representative subset: ties across namespaces and pods, an exact
		// SQL plan, a Go-side filter, and a match rare enough that 60
		// lines span several time chunks.
		pagedQueries := map[string]bool{
			`{namespace=~"shop|pay", level!="info"}`:                   true,
			`{namespace="shop"} | logfmt | status >= 500`:              true,
			`{namespace="pay"} | json | msg="charge failed"`:           true,
			`{cluster="c1", team="payments"} |= "created" or "failed"`: true,
		}
		for _, q := range queries {
			for _, run := range []struct {
				name string
				fwd  bool
				e    *Engine
			}{{"backward", false, f.ch}, {"forward", true, f.ch}, {"backward, 7-row pages", false, f.paged}, {"forward, 7-row pages", true, f.paged}} {
				if run.e == f.paged && !pagedQueries[q] {
					continue
				}
				fwd := run.fwd
				p := QueryParams{Query: q, Start: f.now.Add(-2 * time.Hour), End: f.now.Add(time.Minute), Limit: 60, Forward: fwd}
				got, err := run.e.Query(ctx, p)
				if err != nil {
					t.Fatalf("%s (%s): %v", q, run.name, err)
				}
				want, err := f.mem.Query(ctx, p)
				if err != nil {
					t.Fatalf("%s (reference): %v", q, err)
				}
				if len(want.Lines) == 0 {
					t.Fatalf("%s: reference found nothing; fixture is wrong", q)
				}
				if len(got.Lines) != len(want.Lines) {
					t.Fatalf("%s fwd=%v: %d lines, want %d", q, fwd, len(got.Lines), len(want.Lines))
				}
				for i := range want.Lines {
					// Lines sharing a timestamp may come back in either order.
					if got.Lines[i].TS != want.Lines[i].TS {
						t.Fatalf("%s fwd=%v line %d ts %d, want %d", q, fwd, i, got.Lines[i].TS, want.Lines[i].TS)
					}
				}
				gk, wk := map[string]int{}, map[string]int{}
				for i := range want.Lines {
					gk[lineKey(got.Lines[i])]++
					wk[lineKey(want.Lines[i])]++
				}
				for k, n := range wk {
					if gk[k] != n {
						// The boundary timestamp can hold more lines than the
						// limit takes; compare only strictly inside it.
						last := want.Lines[len(want.Lines)-1].TS
						if !strings.HasPrefix(k, fmt.Sprint(last)+"|") {
							t.Fatalf("%s fwd=%v: line %q differs", q, fwd, k)
						}
					}
				}
			}
		}
	})

	t.Run("metric queries: exact SQL, rollup and Go paths equal the reference", func(t *testing.T) {
		aligned := f.now.Add(-90 * time.Minute)
		cases := []struct {
			q          string
			start, end time.Time
			step       time.Duration
		}{
			{`count_over_time({namespace="shop"}[5m])`, aligned, f.now, time.Minute},
			{`sum by (namespace) (count_over_time({level="error"}[5m]))`, aligned, f.now, time.Minute},
			{`sum by (namespace, level) (rate({namespace=~".+"}[2m]))`, aligned.Add(17 * time.Second), f.now, 30 * time.Second},
			{`sum(bytes_over_time({namespace="pay"}[10m]))`, aligned, f.now, 5 * time.Minute},
			{`sum by (team) (bytes_rate({cluster="c1"}[3m]))`, aligned, f.now, time.Minute},
			{`sum by (s) (count_over_time({namespace="shop"} | logfmt s="status" | s >= 500 [5m]))`, aligned, f.now, time.Minute},
			{`topk(2, sum by (pod) (rate({namespace="shop"}[1m])))`, aligned, f.now, 2 * time.Minute},
			{`sum(count_over_time({namespace="shop"}[1h]))`, f.now, f.now, 0},
			{`count_over_time({namespace="shop"} |= "timeout" [3m] offset 2m)`, aligned, f.now, time.Minute},
			{`sum by (namespace) (count_over_time({level="error"}[5m])) > 10`, aligned, f.now, time.Minute},
			{`absent_over_time({namespace="nope"}[5m])`, aligned, aligned.Add(10 * time.Minute), time.Minute},
			{`sum(count_over_time({namespace="pay"} | json | __error__="" | amount > 150 [10m])) / sum(count_over_time({namespace="pay"}[10m]))`, aligned, f.now, 5 * time.Minute},
		}
		for _, tc := range cases {
			expr, err := logql.ParseSampleExpr(tc.q)
			if err != nil {
				t.Fatal(err)
			}
			want, err := logql.Evaluate(ctx, expr, tc.start, tc.end, tc.step, f.mem.store)
			if err != nil {
				t.Fatalf("%s (reference): %v", tc.q, err)
			}
			for name, eng := range map[string]*Engine{"rollup": f.ch, "raw": f.raw} {
				expr, _ := logql.ParseSampleExpr(tc.q)
				got, err := logql.Evaluate(ctx, expr, tc.start, tc.end, tc.step, eng.store)
				if err != nil {
					t.Fatalf("%s (%s): %v", tc.q, name, err)
				}
				sameSeries(t, tc.q+" ["+name+"]", got.Series, want.Series)
			}
		}
	})

	t.Run("rollup path is exact at minute edges", func(t *testing.T) {
		// Lines sit on every second, so every window has lines exactly on
		// both edges: rollup and raw must agree to the line.
		q := `sum(count_over_time({namespace="shop", workload="api"}[1m]))`
		rollup, err := f.ch.QueryMetric(ctx, q, f.now.Add(-60*time.Minute), f.now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := f.raw.QueryMetric(ctx, q, f.now.Add(-60*time.Minute), f.now, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		sameSeries(t, "rollup vs raw", rollup, raw)
		if rollup[0].Points[5].Value != 60 {
			t.Fatalf("a (t-1m, t] window holds %v lines, want 60", rollup[0].Points[5].Value)
		}
	})

	t.Run("volume: rollup, raw and reference agree", func(t *testing.T) {
		for _, p := range []VolumeParams{
			{Query: `{namespace="shop"}`, Start: f.now.Add(-2 * time.Hour), End: f.now},
			{Query: `{cluster="c1"}`, GroupBy: "namespace", Start: f.now.Add(-2 * time.Hour), End: f.now, Step: 10 * time.Minute},
			{Query: `{namespace="pay"} |= "failed"`, GroupBy: "level", Start: f.now.Add(-time.Hour), End: f.now},
			{Query: `{namespace="shop"} | logfmt`, GroupBy: "status", Start: f.now.Add(-time.Hour), End: f.now},
		} {
			want, err := f.mem.Volume(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			for name, eng := range map[string]*Engine{"rollup": f.ch, "raw": f.raw} {
				got, err := eng.Volume(ctx, p)
				if err != nil {
					t.Fatalf("%s %s: %v", p.Query, name, err)
				}
				if got.TotalLines != want.TotalLines || got.TotalBytes != want.TotalBytes || got.EstCostUSDMonth != want.EstCostUSDMonth {
					t.Fatalf("%s %s totals %d/%d/%v, want %d/%d/%v", p.Query, name, got.TotalLines, got.TotalBytes, got.EstCostUSDMonth,
						want.TotalLines, want.TotalBytes, want.EstCostUSDMonth)
				}
				sameSeries(t, p.Query+" volume "+name, got.Series, want.Series)
			}
		}
	})

	t.Run("patterns, labels, series", func(t *testing.T) {
		p := PatternParams{Query: `{namespace="shop"}`, Start: f.now.Add(-2 * time.Hour), End: f.now}
		got, err := f.ch.Patterns(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		want, err := f.mem.Patterns(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		// The sample line depends on the order of equal-timestamp lines,
		// which ClickHouse does not fix; everything else must match.
		strip := func(ps []PatternOut) string {
			var b strings.Builder
			for _, p := range ps {
				fmt.Fprintf(&b, "%s|%d|%s|%.6f|%v\n", p.Pattern, p.Count, p.Level, p.SharePct, p.Trend)
			}
			return b.String()
		}
		if len(got.Patterns) == 0 || strip(got.Patterns) != strip(want.Patterns) || got.LinesAnalyzed != want.LinesAnalyzed {
			t.Fatalf("patterns differ:\n got %s\nwant %s", strip(got.Patterns), strip(want.Patterns))
		}
		names, err := f.ch.LabelNames(ctx, "", "", f.now.Add(-2*time.Hour), f.now)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(names) != "[app cluster container level namespace node pod stream team workload workload_kind]" {
			t.Fatalf("label names = %v", names)
		}
		for name, eng := range map[string]*Engine{"rollup": f.ch, "raw": f.raw} {
			vals, err := eng.LabelValues(ctx, "namespace", "", "", f.now.Add(-2*time.Hour), f.now, 0)
			if err != nil || fmt.Sprint(vals) != "[pay shop]" {
				t.Fatalf("namespace values (%s) = %v %v", name, vals, err)
			}
		}
		apps, err := f.ch.LabelValues(ctx, "app", `{namespace="shop"}`, "", f.now.Add(-2*time.Hour), f.now, 0)
		if err != nil || fmt.Sprint(apps) != "[api]" {
			t.Fatalf("app values = %v %v", apps, err)
		}
		series, err := f.ch.Series(ctx, []string{`{namespace="shop", workload="api"}`}, "c1", f.now.Add(-2*time.Hour), f.now)
		if err != nil || len(series) != 6 { // 2 pods × 3 levels
			t.Fatalf("series = %v %v", series, err)
		}
	})
}

func TestTailAgainstClickHouse(t *testing.T) {
	db := chtest.Open(t, "kh_ingest_tail")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	w := &clickhouse.SignalWriter{Conn: db.Native}
	e := New(Options{Store: &CHStore{Conn: db.Native}, TailPoll: 100 * time.Millisecond})
	write := func(body string, age time.Duration) {
		if err := w.WriteLogs(ctx, []clickhouse.LogRow{{TS: time.Now().UTC().Add(-age), OrgID: "default", ClusterID: "c1",
			Namespace: "shop", Pod: "p", Body: body}}); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan string, 100)
	tailCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- e.Tail(tailCtx, TailParams{Query: `{namespace="shop"} |= "tail"`, ClusterID: "c1"}, func(b TailBatch) error {
			for _, l := range b.Lines {
				got <- l.Body
			}
			return nil
		})
	}()
	time.Sleep(300 * time.Millisecond)
	write("tail one", 0)
	write("tail late", 2*time.Second) // arrives after its timestamp
	write("ignored", 0)
	write("tail two", 0)
	want := map[string]bool{"tail one": true, "tail late": true, "tail two": true}
	deadline := time.After(10 * time.Second)
	for len(want) > 0 {
		select {
		case b := <-got:
			if !want[b] {
				t.Fatalf("unexpected or repeated line %q", b)
			}
			delete(want, b)
		case <-deadline:
			t.Fatalf("tail never delivered %v", want)
		}
	}
	time.Sleep(500 * time.Millisecond) // more polls: nothing may repeat
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		t.Fatalf("repeated line %q", b)
	default:
	}
}
