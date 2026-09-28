// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestPlanSelector(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		where    string
		args     []any
		goStages int
		rollup   bool
	}{
		{
			name:  "matchers",
			query: `{namespace="shop", app=~"a|b", pod!~"x.*", team!="", cluster=~".*", node=~".+", level!~".*"}`,
			where: "(namespace = ?) AND (labels[?] IN (?, ?)) AND (NOT (match(pod, ?))) AND (team != ?) AND (1) AND (node != '') AND (0)",
			args:  []any{"shop", "app", "a", "b", "^(?:x.*)$", ""},
		},
		{
			name:   "rollup selector",
			query:  `{namespace="shop", level=~"error|fatal", cluster="c1"}`,
			where:  "(namespace = ?) AND (level IN (?, ?)) AND (cluster_id = ?)",
			args:   []any{"shop", "error", "fatal", "c1"},
			rollup: true,
		},
		{
			name:   "single literal regex is equality",
			query:  `{namespace=~"shop"}`,
			where:  "namespace = ?",
			args:   []any{"shop"},
			rollup: true,
		},
		{
			name:  "line filters",
			query: `{namespace="shop"} |= "50%_off\\" != "debug" |~ "time(out)?" |~ "plain" |~ "(?i)Error" |= "a" or "b" !~ "x|y"`,
			where: `(namespace = ?) AND (like(body, ?)) AND (NOT (like(body, ?))) AND (match(body, ?)) AND (like(body, ?)) AND (ilike(body, ?)) AND ((like(body, ?)) OR (like(body, ?))) AND (NOT (match(body, ?)))`,
			args:  []any{"shop", `%50\%\_off\\%`, "%debug%", "time(out)?", "%plain%", "%Error%", "%a%", "%b%", "x|y"},
		},
		{
			name:     "stream label filter pushed, parsed filter prefiltered",
			query:    `{namespace="shop"} | level="error" | json | status >= 500 | msg="boom"`,
			where:    "(namespace = ?) AND (level = ?) AND ((labels[?] = ?) OR ((labels[?] = '') AND (JSONExtractString(body, ?) = ?)))",
			args:     []any{"shop", "error", "msg", "boom", "msg", "msg", "boom"},
			goStages: 3,
		},
		{
			name:     "json path param",
			query:    `{namespace="shop"} | json m="req.method" | m="GET"`,
			where:    "(namespace = ?) AND ((labels[?] = ?) OR ((labels[?] = '') AND (JSONExtractString(body, ?, ?) = ?)))",
			args:     []any{"shop", "m", "GET", "m", "req", "method", "GET"},
			goStages: 2,
		},
		{
			name:     "json numeric equality has no body prefilter",
			query:    `{namespace="shop"} | json | code="500"`,
			where:    "(namespace = ?) AND ((labels[?] = ?) OR (labels[?] = ''))",
			args:     []any{"shop", "code", "500", "code"},
			goStages: 2,
		},
		{
			name:     "json on a column label",
			query:    `{app="x"} | json | pod="p1"`,
			where:    "(labels[?] = ?) AND ((pod = ?) OR ((pod = '') AND (JSONExtractString(body, ?) = ?)))",
			args:     []any{"app", "x", "p1", "pod", "p1"},
			goStages: 2,
		},
		{
			name:     "logfmt prefilter",
			query:    `{app="x"} | logfmt | method="GET"`,
			where:    `(labels[?] = ?) AND ((labels[?] = ?) OR ((labels[?] = '') AND ((position(body, ?) > 0) OR (position(body, ?) > 0))))`,
			args:     []any{"app", "x", "method", "GET", "method", "method=GET", `method="GET"`},
			goStages: 2,
		},
		{
			name:     "line filter after a parser still sees the body",
			query:    `{app="x"} | json |= "timeout"`,
			where:    "(labels[?] = ?) AND (like(body, ?))",
			args:     []any{"app", "x", "%timeout%"},
			goStages: 2,
		},
		{
			name:     "line filter after line_format stays in Go",
			query:    `{app="x"} | json | line_format "{{.m}}" |= "timeout"`,
			where:    "labels[?] = ?",
			args:     []any{"app", "x"},
			goStages: 3,
		},
		{
			name:     "or with an unprefilterable side",
			query:    `{app="x"} | json | msg="a" or status >= 500`,
			where:    "labels[?] = ?",
			args:     []any{"app", "x"},
			goStages: 2,
		},
		{
			name:     "and keeps the prefilterable side",
			query:    `{app="x"} | json | msg="a" and status >= 500`,
			where:    "(labels[?] = ?) AND ((labels[?] = ?) OR ((labels[?] = '') AND (JSONExtractString(body, ?) = ?)))",
			args:     []any{"app", "x", "msg", "a", "msg", "msg", "a"},
			goStages: 2,
		},
		{
			name:     "label_format blocks label prefilters",
			query:    `{app="x"} | json | label_format msg="{{.other}}" | msg="a"`,
			where:    "labels[?] = ?",
			args:     []any{"app", "x"},
			goStages: 3,
		},
		{
			name:  "error filter before parsing",
			query: `{app="x"} | __error__="" | __error__!=""`,
			where: "(labels[?] = ?) AND (1) AND (0)",
			args:  []any{"app", "x"},
		},
		{
			name:   "empty selector",
			query:  `{}`,
			where:  "1",
			rollup: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sel, err := ParseLogSelector(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			p := PlanSelector(sel)
			sql, args := p.WhereSQL()
			if sql != tc.where {
				t.Fatalf("where\n got %s\nwant %s", sql, tc.where)
			}
			if len(args) != len(tc.args) || (len(args) > 0 && !reflect.DeepEqual(args, tc.args)) {
				t.Fatalf("args\n got %#v\nwant %#v", args, tc.args)
			}
			if len(p.Stages) != tc.goStages {
				t.Fatalf("go stages = %d (%v), want %d", len(p.Stages), p.Stages, tc.goStages)
			}
			if p.Exact() != (tc.goStages == 0) {
				t.Fatal("Exact() disagrees with the remaining stages")
			}
			if p.RollupOK != tc.rollup {
				t.Fatalf("RollupOK = %v, want %v", p.RollupOK, tc.rollup)
			}
			if n := strings.Count(sql, "?"); n != len(args) {
				t.Fatalf("%d placeholders for %d args", n, len(args))
			}
		})
	}
}

func TestStatementBuilders(t *testing.T) {
	sel, _ := ParseLogSelector(`{namespace="shop", app="api"}`)
	p := PlanSelector(sel)
	g, err := NewGrid(time.Unix(600, 0), time.Unix(1200, 0), time.Minute, 5*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	check := func(name string, q Query) {
		t.Helper()
		if n := strings.Count(q.SQL, "?"); n != len(q.Args) {
			t.Fatalf("%s: %d placeholders, %d args\n%s\n%#v", name, n, len(q.Args), q.SQL, q.Args)
		}
	}

	q := p.BucketQuery("default", g, []string{"namespace", "app"}, 1000)
	check("bucket", q)
	want := "SELECT namespace AS l0, labels[?] AS l1, intDiv(toUnixTimestamp64Nano(ts) - toInt64(?), toInt64(?)) AS b, count() AS c, sum(length(body)) AS bs FROM logs WHERE org_id = ? AND ts > fromUnixTimestamp64Nano(toInt64(?)) AND ts <= fromUnixTimestamp64Nano(toInt64(?)) AND ((namespace = ?) AND (labels[?] = ?)) GROUP BY l0, l1, b LIMIT ?"
	if q.SQL != want {
		t.Fatalf("bucket sql\n got %s\nwant %s", q.SQL, want)
	}
	wantArgs := []any{"app", g.Origin + 1, g.Width, "default", g.DataStart(), g.DataEnd(), "shop", "app", "api", 1000}
	if fmt.Sprint(q.Args) != fmt.Sprint(wantArgs) {
		t.Fatalf("bucket args %v, want %v", q.Args, wantArgs)
	}

	full := p.BucketQuery("default", g, nil, 10)
	check("bucket full identity", full)
	if !strings.HasPrefix(full.SQL, "SELECT cluster_id, namespace, workload, workload_kind, pod, container, node, team, stream, level, labels, intDiv") ||
		!strings.Contains(full.SQL, "GROUP BY cluster_id, namespace, workload, workload_kind, pod, container, node, team, stream, level, labels, b") {
		t.Fatalf("full identity sql: %s", full.SQL)
	}

	rsel, _ := ParseLogSelector(`{namespace="shop", level="error"}`)
	rp := PlanSelector(rsel)
	if !rp.RollupEligible(g, []string{"namespace"}) || rp.RollupEligible(g, nil) || rp.RollupEligible(g, []string{"pod"}) {
		t.Fatal("rollup eligibility wrong")
	}
	odd, _ := NewGrid(time.Unix(630, 0), time.Unix(1230, 0), time.Minute, 5*time.Minute, 0)
	if rp.RollupEligible(odd, []string{"namespace"}) {
		t.Fatal("a grid not aligned to minutes cannot use the rollup")
	}
	rq := rp.RollupBucketQuery("default", g, []string{}, 100)
	check("rollup no labels", rq)
	if !strings.HasPrefix(rq.SQL, "SELECT intDiv(") || !strings.Contains(rq.SQL, "GROUP BY b LIMIT ?") {
		t.Fatalf("rollup sql: %s", rq.SQL)
	}
	check("rollup", rp.RollupBucketQuery("default", g, []string{"namespace", "level"}, 100))

	check("logs", p.LogsQuery("default", 1, 2, false, 100))
	check("count", p.CountQuery("default", 1, 2))
	check("volume raw", p.VolumeQuery("default", 0, 3600e9, 60e9, "app", false))
	check("volume rollup", rp.VolumeQuery("default", 0, 3600e9, 60e9, "level", true))
	check("label names", p.LabelNamesQuery("default", 1, 2, 1000, 100))
	check("label values map", p.LabelValuesQuery("default", "app", 1, 2, 100, false))
	check("label values rollup", rp.LabelValuesQuery("default", "namespace", 1, 2, 100, true))
	if strings.Contains(p.LogsQuery("default", 1, 2, true, 5).SQL, "DESC") {
		t.Fatal("forward queries must sort ascending")
	}
}

// Nothing a user types reaches the SQL text.
func TestUserInputNeverInSQL(t *testing.T) {
	evil := `x'); DROP TABLE logs; --`
	sel, err := ParseLogSelector(fmt.Sprintf(`{namespace=%q, %s=%q} |= %q |~ %q | json | msg=%q`,
		evil, "app", evil, evil, `x'\)`, evil))
	if err != nil {
		t.Fatal(err)
	}
	p := PlanSelector(sel)
	sql, _ := p.WhereSQL()
	for _, q := range []string{sql, p.LogsQuery("default", 0, 1, false, 1).SQL, p.LabelValuesQuery("default", "app", 0, 1, 1, false).SQL} {
		if strings.Contains(q, "DROP") || strings.Contains(q, "'x") {
			t.Fatalf("user input leaked into SQL: %s", q)
		}
	}
}
