// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logql

import (
	"strings"
	"testing"
	"time"
)

// Round trip: every query parses, renders to the expected canonical
// form, and the canonical form parses to the same rendering.
func TestParseCanonical(t *testing.T) {
	tests := []struct{ in, want string }{
		{`{app="foo"}`, `{app="foo"}`},
		{`{}`, `{}`},
		{`{ app = "foo" , env!="dev", pod=~"api-.*", ns!~"kube-.*" }`, `{app="foo", env!="dev", pod=~"api-.*", ns!~"kube-.*"}`},
		{"{app=`raw\\d`}", `{app="raw\\d"}`},
		{`{app="foo"} |= "err" != "debug" |~ "time(out)?" !~ "(?i)health"`, `{app="foo"} |= "err" != "debug" |~ "time(out)?" !~ "(?i)health"`},
		{`{app="foo"} |= "a" or "b"`, `{app="foo"} |= "a" or "b"`},
		{`{app="foo"} | json`, `{app="foo"} | json`},
		{`{app="foo"} | json status, ua="request.headers[\"User-Agent\"]", first="servers[0]"`, `{app="foo"} | json status, ua="request.headers[\"User-Agent\"]", first="servers[0]"`},
		{`{app="foo"} | logfmt --strict --keep-empty host, ip="fwd"`, `{app="foo"} | logfmt --strict --keep-empty host, ip="fwd"`},
		{`{app="foo"} | regexp "(?P<status>\\d{3})"`, `{app="foo"} | regexp "(?P<status>\\d{3})"`},
		{`{app="foo"} | pattern "<ip> - <_> \"<method> <path>\""`, `{app="foo"} | pattern "<ip> - <_> \"<method> <path>\""`},
		{`{app="foo"} | unpack | decolorize`, `{app="foo"} | unpack | decolorize`},
		{`{app="foo"} | json | status >= 500`, `{app="foo"} | json | status >= 500`},
		{`{app="foo"} | logfmt | duration > 250ms and size <= 10KB`, `{app="foo"} | logfmt | (duration > 250ms and size <= 10KB)`},
		{`{app="foo"} | json | status >= 500, method="GET"`, `{app="foo"} | json | (status >= 500 and method="GET")`},
		{`{app="foo"} | json | status >= 500 method="GET"`, `{app="foo"} | json | (status >= 500 and method="GET")`},
		{`{app="foo"} | json | level="error" or status == 500 and x != 1`, `{app="foo"} | json | (level="error" or (status == 500 and x != 1))`},
		{`{app="foo"} | json | (level="error" or level="warn") and status > -1`, `{app="foo"} | json | ((level="error" or level="warn") and status > -1)`},
		{`{app="foo"} | json | level=~"err.*" | level!~"x"`, `{app="foo"} | json | level=~"err.*" | level!~"x"`},
		{`{app="foo"} | __error__=""`, `{app="foo"} | __error__=""`},
		{`{app="foo"} | line_format "{{.level}}: {{__line__}}"`, `{app="foo"} | line_format "{{.level}}: {{__line__}}"`},
		{`{app="foo"} | label_format lvl=level, msg="{{.a}}-{{.b}}"`, `{app="foo"} | label_format lvl=level, msg="{{.a}}-{{.b}}"`},
		{`{app="foo"} | drop a, b="x", c=~"y.*" | keep level, app`, `{app="foo"} | drop a, b="x", c=~"y.*" | keep level, app`},
		{`count_over_time({app="foo"}[5m])`, `count_over_time({app="foo"} [5m])`},
		{`count_over_time({app="foo"} |= "err" [5m])`, `count_over_time({app="foo"} |= "err" [5m])`},
		{`count_over_time({app="foo"}[5m] | json | status >= 500)`, `count_over_time({app="foo"} | json | status >= 500 [5m])`},
		{`count_over_time(({app="foo"} |= "x")[1h30m] offset 5m)`, `count_over_time({app="foo"} |= "x" [1h30m] offset 5m)`},
		{`rate({app="foo"}[1m])`, `rate({app="foo"} [1m])`},
		{`bytes_rate({app="foo"}[30s])`, `bytes_rate({app="foo"} [30s])`},
		{`absent_over_time({app="foo"}[1d])`, `absent_over_time({app="foo"} [1d])`},
		{`sum by (namespace) (count_over_time({level="error"}[5m]))`, `sum by (namespace)(count_over_time({level="error"} [5m]))`},
		{`sum(count_over_time({level="error"}[5m])) by (namespace, pod)`, `sum by (namespace, pod)(count_over_time({level="error"} [5m]))`},
		{`sum without (pod) (rate({app="a"}[5m]))`, `sum without (pod)(rate({app="a"} [5m]))`},
		{`topk(5, sum by (app) (rate({app=~".+"}[5m])))`, `topk(5, sum by (app)(rate({app=~".+"} [5m])))`},
		{`sum by (namespace) (count_over_time({level="error"}[5m])) > 100`, `(sum by (namespace)(count_over_time({level="error"} [5m])) > 100)`},
		{`rate({a="b"}[5m]) * 60`, `(rate({a="b"} [5m]) * 60)`},
		{`1 + 2 * 3`, `(1 + (2 * 3))`},
		{`2 ^ 3 ^ 2`, `(2 ^ (3 ^ 2))`},
		{`-2 ^ 2`, `(-1 * (2 ^ 2))`},
		{`vector(1)+vector(1)`, `(vector(1) + vector(1))`},
		{`rate({a="b"}[5m]) > bool 1`, `(rate({a="b"} [5m]) > bool 1)`},
		{`rate({a="b"}[5m]) / on (app) rate({a="c"}[5m])`, `(rate({a="b"} [5m]) / on (app) rate({a="c"} [5m]))`},
		{`rate({a="b"}[5m]) or vector(0)`, `(rate({a="b"} [5m]) or vector(0))`},
		{`count_over_time({a="b"}[5m]) unless count_over_time({a="c"}[5m])`, `(count_over_time({a="b"} [5m]) unless count_over_time({a="c"} [5m]))`},
		{"{app=\"foo\"} # comment\n |= \"x\"", `{app="foo"} |= "x"`},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			e, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.in, err)
			}
			got := e.String()
			if got != tc.want {
				t.Fatalf("Parse(%q).String()\n got %s\nwant %s", tc.in, got, tc.want)
			}
			again, err := Parse(got)
			if err != nil {
				t.Fatalf("canonical form does not parse: %v", err)
			}
			if again.String() != got {
				t.Fatalf("round trip changed: %s → %s", got, again.String())
			}
		})
	}
}

func TestParseTree(t *testing.T) {
	e, err := Parse(`topk(3, sum by (namespace) (rate({cluster="c1", level=~"error|fatal"} |= "timeout" | json | status >= 500 [5m] offset 1m)))`)
	if err != nil {
		t.Fatal(err)
	}
	top := e.(*VectorAggExpr)
	if top.Op != VecTopk || top.Param != 3 {
		t.Fatalf("top = %+v", top)
	}
	sum := top.Inner.(*VectorAggExpr)
	if sum.Op != VecSum || sum.Grouping.Without || len(sum.Grouping.Labels) != 1 || sum.Grouping.Labels[0] != "namespace" {
		t.Fatalf("sum = %+v", sum)
	}
	r := sum.Inner.(*RangeAggExpr)
	if r.Op != RangeRate || r.Range != 5*time.Minute || r.Offset != time.Minute {
		t.Fatalf("range = %+v", r)
	}
	ms := r.Selector.Matchers
	if len(ms) != 2 || ms[1].Type != MatchRegexp || !ms[1].Matches("fatal") || ms[1].Matches("errors") {
		t.Fatalf("matchers = %v (regex must be anchored)", ms)
	}
	if len(r.Selector.Stages) != 3 {
		t.Fatalf("stages = %v", r.Selector.Stages)
	}
	nf := r.Selector.Stages[2].(*LabelFilterStage).Filter.(*NumericLabelFilter)
	if nf.Name != "status" || nf.Op != CmpGte || nf.Value != 500 || nf.Kind != KindNumber {
		t.Fatalf("numeric filter = %+v", nf)
	}
}

func TestParseValueKinds(t *testing.T) {
	tests := []struct {
		q     string
		kind  ValueKind
		value float64
	}{
		{`{a="b"} | x > 1.5`, KindNumber, 1.5},
		{`{a="b"} | x > 1e3`, KindNumber, 1000},
		{`{a="b"} | x > 0x10`, KindNumber, 16},
		{`{a="b"} | x > 250ms`, KindDuration, 0.25},
		{`{a="b"} | x > 1h30m`, KindDuration, 5400},
		{`{a="b"} | x > 1.5s`, KindDuration, 1.5},
		{`{a="b"} | x > 10KB`, KindBytes, 10000},
		{`{a="b"} | x > 2KiB`, KindBytes, 2048},
		{`{a="b"} | x > 1.5MB`, KindBytes, 1.5e6},
	}
	for _, tc := range tests {
		e, err := ParseLogSelector(tc.q)
		if err != nil {
			t.Fatalf("%s: %v", tc.q, err)
		}
		f := e.Stages[0].(*LabelFilterStage).Filter.(*NumericLabelFilter)
		if f.Kind != tc.kind || f.Value != tc.value {
			t.Errorf("%s: kind=%v value=%v, want %v %v", tc.q, f.Kind, f.Value, tc.kind, tc.value)
		}
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		q, want string
		line    int
		col     int
	}{
		{`{app="foo"`, "expected ',' or '}' after a matcher, found end of query", 1, 11},
		{`{app=foo}`, `expected a quoted string after app=, found "foo"`, 1, 6},
		{`{app~"x"}`, `unexpected character '~'`, 1, 5},
		{`{app="x"} |= foo`, `expected a quoted string after |=, found "foo"`, 1, 14},
		{`{app=~"("}`, "invalid regular expression", 1, 7},
		{`{app="x"} |~ "a(b"`, "invalid regular expression", 1, 14},
		{`count_over_time({app="x"})`, "expected a range like [5m]", 1, 26},
		{`count_over_time({app="x"}[5])`, "expected a duration like 5m", 1, 27},
		{`{app="x"}[5m]`, "must be inside a function", 1, 10},
		{`sum_over_time({app="x"} | unwrap b [5m])`, "unwrap", 1, 1},
		{`topk(0, rate({a="b"}[5m]))`, "integer k between 1", 1, 6},
		{`topk(rate({a="b"}[5m]))`, "integer k between 1", 1, 6},
		{`sum({a="b"})`, "needs a metric query inside", 1, 5},
		{`rate({a="b"}[5m]) + {a="b"}`, "needs metric queries on both sides", 1, 19},
		{`frobnicate({a="b"})`, `unknown function "frobnicate"`, 1, 1},
		{`{a="b"} | json | status >`, "expected a number, duration", 1, 26},
		{`{a="b"} | regexp "(\\d+)"`, "named capture group", 1, 18},
		{`{a="b"} | pattern "<a><b>"`, "adjacent", 1, 19},
		{`{a="b"} | line_format "{{.a"`, "invalid line_format template", 1, 23},
		{"{a=\"b\"}\n  |= \"x\" |= ", "expected a quoted string after |=, found end of query", 2, 13},
		{`{a="b"} | logfmt --loose`, "unknown logfmt flag", 1, 20},
		{`{a="\q"}`, "invalid escape", 1, 4},
		{`{a="b"} and 1`, "needs metric queries on both sides", 1, 9},
		{`1 > 2`, "must use the bool modifier", 1, 3},
		{`rate({a="b"}[5m]) + on(x) group_left rate({a="c"}[5m])`, "group_left", 1, 27},
		{"{a=\"b\"} |= \"x\x00\"", "NUL", 1, 14},
		{`{a="b"} |= "x\x00"`, "NUL byte in string", 1, 12},
	}
	for _, tc := range tests {
		t.Run(tc.q, func(t *testing.T) {
			_, err := Parse(tc.q)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error containing %q", tc.q, tc.want)
			}
			pe, ok := err.(*ParseError)
			if !ok {
				t.Fatalf("error %T %v is not a *ParseError", err, err)
			}
			if !strings.Contains(pe.Msg, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", pe.Msg, tc.want)
			}
			if pe.Line != tc.line || pe.Col != tc.col {
				t.Fatalf("error at %d:%d, want %d:%d (%s)", pe.Line, pe.Col, tc.line, tc.col, pe.Msg)
			}
		})
	}
}

func TestParseLabels(t *testing.T) {
	m, err := ParseLabels(`{namespace="shop", pod="api-1", app="a\"b"}`)
	if err != nil {
		t.Fatal(err)
	}
	if m["namespace"] != "shop" || m["pod"] != "api-1" || m["app"] != `a"b` {
		t.Fatalf("labels = %v", m)
	}
	if _, err := ParseLabels(`{a=~"x"}`); err == nil {
		t.Fatal("regex matcher must be rejected in a label set")
	}
}

func TestParseDuration(t *testing.T) {
	tests := map[string]time.Duration{
		"5m": 5 * time.Minute, "1h30m": 90 * time.Minute, "1d": 24 * time.Hour, "2w": 14 * 24 * time.Hour,
		"250ms": 250 * time.Millisecond, "1.5h": 90 * time.Minute, "10us": 10 * time.Microsecond,
	}
	for in, want := range tests {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "5", "m", "5x", "1h-"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) should fail", bad)
		}
	}
	if FormatDuration(90*time.Minute) != "1h30m" || FormatDuration(36*time.Hour) != "1d12h" || FormatDuration(1500*time.Millisecond) != "1s500ms" {
		t.Fatal("FormatDuration")
	}
}
