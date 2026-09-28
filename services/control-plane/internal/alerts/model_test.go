// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
)

func TestParseQuery(t *testing.T) {
	cases := []struct {
		kind, q string
		want    Query
	}{
		{KindCost, `cost{namespace="ml-inference"}`, Query{Metric: "cost",
			Matchers: []Matcher{{"namespace", chsql.OpEq, "ml-inference"}}, Range: 15 * time.Minute}},
		{KindCost, `cost by (team)`, Query{Metric: "cost", Range: 15 * time.Minute, By: []string{"team"}}},
		{KindCost, `cost{team=~"pay.*", namespace!="kube-system",}[30m] by (team, namespace)`, Query{Metric: "cost",
			Matchers: []Matcher{{"team", chsql.OpRe, "pay.*"}, {"namespace", chsql.OpNeq, "kube-system"}},
			Range:    30 * time.Minute, By: []string{"team", "namespace"}}},
		{KindNetwork, `network{namespace="edge", egress="true"} by (workload)`, Query{Metric: "network",
			Matchers: []Matcher{{"namespace", chsql.OpEq, "edge"}, {"egress", chsql.OpEq, "true"}},
			Range:    15 * time.Minute, By: []string{"workload"}}},
		{KindEvent, `events{kind="oom_killed",namespace="prod"}[10m]`, Query{Metric: "events",
			Matchers: []Matcher{{"kind", chsql.OpEq, "oom_killed"}, {"namespace", chsql.OpEq, "prod"}}, Range: 10 * time.Minute}},
		{KindEvent, `events{kind!~"warning|restarted"}`, Query{Metric: "events",
			Matchers: []Matcher{{"kind", chsql.OpNre, "warning|restarted"}}, Range: 10 * time.Minute}},
		{KindAnomaly, `anomaly{kind="spend"}`, Query{Metric: "anomaly", Matchers: []Matcher{{"kind", chsql.OpEq, "spend"}}}},
		{KindAnomaly, `anomaly`, Query{Metric: "anomaly"}},
		{KindEvent, "events{reason=\"Back\\\"Off\"}[1d]", Query{Metric: "events",
			Matchers: []Matcher{{"reason", chsql.OpEq, `Back"Off`}}, Range: 24 * time.Hour}},
	}
	for _, c := range cases {
		got, err := ParseQuery(c.kind, c.q)
		if err != nil {
			t.Errorf("ParseQuery(%q): %v", c.q, err)
			continue
		}
		if !reflect.DeepEqual(*got, c.want) {
			t.Errorf("ParseQuery(%q) = %+v\nwant %+v", c.q, *got, c.want)
		}
	}
}

func TestParseQueryErrors(t *testing.T) {
	for _, c := range []struct{ kind, q, want string }{
		{KindCost, `network{}`, "expects a cost"},
		{KindCost, `cost{namespace=prod}`, "double-quoted"},
		{KindCost, `cost{namespace~"x"}`, "expected =, !=, =~ or !~"},
		{KindCost, `cost{secret="x"}`, "not available on cost"},
		{KindCost, `cost by (secret)`, "cannot group"},
		{KindCost, `cost{namespace="x"`, "expected ','"},
		{KindCost, `cost[5x]`, "duration"},
		{KindCost, `cost[48h]`, "range longer"},
		{KindCost, `cost sum`, "expected by"},
		{KindCost, `cost by (team) extra`, "unexpected"},
		{KindCost, `cost{namespace=~"(unclosed"}`, "invalid regex"},
		{KindNetwork, `network{egress=~"t.*"}`, "= and != only"},
		{KindNetwork, `network{egress="maybe"}`, "true or false"},
		{KindAnomaly, `anomaly by (kind)`, "one series per anomaly"},
		{KindLogs, `x`, "no expression grammar"},
		{KindCost, `cost{namespace="` + strings.Repeat("a", 600) + `"}`, "longer than 512"},
	} {
		_, err := ParseQuery(c.kind, c.q)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ParseQuery(%q) = %v, want error containing %q", c.q, err, c.want)
		}
	}
	// Errors carry a position.
	if _, err := ParseQuery(KindCost, `cost{namespace="a" team="b"}`); err == nil || !strings.Contains(err.Error(), "position 20") {
		t.Errorf("position: %v", err)
	}
}

func TestParseBudgetQuery(t *testing.T) {
	for in, want := range map[string]BudgetQuery{
		"prod-monthly":      {Policy: "prod-monthly", Window: time.Hour},
		"prod-monthly[30m]": {Policy: "prod-monthly", Window: 30 * time.Minute},
		"*":                 {Policy: "*", Window: time.Hour},
		" *[1d] ":           {Policy: "*", Window: 24 * time.Hour},
	} {
		got, err := ParseBudgetQuery(in)
		if err != nil || *got != want {
			t.Errorf("ParseBudgetQuery(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "Prod", "a b", "x[1m]", "x[30d]", "x[1h", "x;drop"} {
		if _, err := ParseBudgetQuery(bad); err == nil {
			t.Errorf("ParseBudgetQuery(%q) should fail", bad)
		}
	}
}

type fakeChannels struct{}

func (fakeChannels) Validate(ch string) error {
	if strings.HasPrefix(ch, "bad://") {
		return errors.New("no provider")
	}
	return nil
}

func validRule() *Rule {
	return &Rule{Name: "High spend", Kind: "cost", Query: `cost by (namespace)`, Op: ">", Threshold: 10}
}

func TestRuleNormalize(t *testing.T) {
	r := validRule()
	r.Channels = []string{" slack://hooks.slack.com/x ", ""}
	if err := r.Normalize(fakeChannels{}); err != nil {
		t.Fatal(err)
	}
	if r.EvalInterval != time.Minute || r.Severity != "warn" || len(r.Channels) != 1 || r.Channels[0] != "slack://hooks.slack.com/x" {
		t.Fatalf("defaults: %+v", r)
	}
	for name, mut := range map[string]func(*Rule){
		"name":        func(r *Rule) { r.Name = "" },
		"name chars":  func(r *Rule) { r.Name = `x"{{ $value }}"` },
		"kind":        func(r *Rule) { r.Kind = "metrics" },
		"op":          func(r *Rule) { r.Op = "=>" },
		"interval":    func(r *Rule) { r.EvalInterval = time.Second },
		"pending":     func(r *Rule) { r.PendingFor = 48 * time.Hour },
		"severity":    func(r *Rule) { r.Severity = "page" },
		"channel":     func(r *Rule) { r.Channels = []string{"bad://x"} },
		"label":       func(r *Rule) { r.Labels = map[string]string{"alertname": "x"} },
		"annotation":  func(r *Rule) { r.Annotations = map[string]string{"bad-key": "x"} },
		"empty query": func(r *Rule) { r.Query = "  " },
	} {
		r := validRule()
		mut(r)
		if err := r.Normalize(fakeChannels{}); err == nil {
			t.Errorf("%s: want validation error", name)
		}
	}
}

func TestRender(t *testing.T) {
	labels := map[string]string{"namespace": "shop", "team": "payments"}
	for tmpl, want := range map[string]string{
		"{{ $value }}":                         "12.35",
		"{{$labels.namespace}} over budget":    "shop over budget",
		`{{ $value | printf "%.1f" }} $/h`:     "12.3 $/h",
		`{{ $value | printf "%d" }}`:           "12",
		"{{ $labels.missing }}|":               "|",
		`{{ $value | printf "%s%s" }}`:         "12.35", // unsafe format ignored
		"{{ .Env }} {{ template \"x\" }} left": "{{ .Env }} {{ template \"x\" }} left",
	} {
		if got := Render(tmpl, 12.3456, labels); got != want {
			t.Errorf("Render(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

func TestSilence(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	s := &Silence{Matchers: map[string]string{"alertname": "High spend", "namespace": "shop"}, EndsAt: now.Add(time.Hour)}
	if err := s.Validate(now); err != nil || !s.StartsAt.Equal(now) {
		t.Fatalf("validate: %v", err)
	}
	if !s.Active(now) || s.Active(now.Add(time.Hour)) {
		t.Fatal("active window")
	}
	if !s.Matches(map[string]string{"alertname": "High spend", "namespace": "shop", "x": "y"}) ||
		s.Matches(map[string]string{"alertname": "High spend"}) {
		t.Fatal("matching")
	}
	for _, bad := range []*Silence{
		{EndsAt: now.Add(time.Hour)},
		{Matchers: map[string]string{"a": "b"}, EndsAt: now.Add(-time.Minute)},
		{Matchers: map[string]string{"a": "b"}, EndsAt: now.Add(100 * 24 * time.Hour)},
		{Matchers: map[string]string{"bad-key": "b"}, EndsAt: now.Add(time.Hour)},
	} {
		if err := bad.Validate(now); err == nil {
			t.Errorf("silence %+v should be invalid", bad)
		}
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, "5m": 5 * time.Minute, "90s": 90 * time.Second, "2d": 48 * time.Hour} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v %v", in, got, err)
		}
	}
	if _, err := ParseDuration("soon"); err == nil {
		t.Error("bad duration")
	}
	if FormatDuration(90*time.Minute) != "90m" || FormatDuration(48*time.Hour) != "2d" || FormatDuration(time.Hour) != "1h" {
		t.Error("format")
	}
}
