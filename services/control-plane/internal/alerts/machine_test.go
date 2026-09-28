// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

func sample(ns string, v float64) Sample {
	return Sample{Labels: map[string]string{"namespace": ns}, Value: v}
}

func stepRule(pending time.Duration) *Rule {
	return &Rule{ID: "r1", Name: "High spend", Kind: KindCost, Query: "cost by (namespace)", Op: ">", Threshold: 10,
		PendingFor: pending, Severity: "critical", Labels: map[string]string{"team": "finops"},
		Annotations: map[string]string{"summary": "{{ $labels.namespace }} at ${{ $value }}/h"}}
}

func TestStepPendingFiringResolved(t *testing.T) {
	r := stepRule(5 * time.Minute)

	// t0: condition true → pending, no notification.
	res := Step(r, nil, []Sample{sample("shop", 12), sample("web", 3)}, t0)
	if len(res.Alerts) != 1 || res.Alerts[0].State != StatePending || len(res.Transitions) != 0 {
		t.Fatalf("t0: %+v", res)
	}
	a := res.Alerts[0]
	if a.Labels["alertname"] != "High spend" || a.Labels["severity"] != "critical" || a.Labels["team"] != "finops" ||
		a.Summary != "shop at $12/h" || a.ID != AlertID("r1", a.Fingerprint) {
		t.Fatalf("alert: %+v", a)
	}

	// t0+3m: still pending (held 3m < 5m).
	res = Step(r, res.Alerts, []Sample{sample("shop", 13)}, t0.Add(3*time.Minute))
	if res.Alerts[0].State != StatePending || len(res.Transitions) != 0 {
		t.Fatalf("t+3m: %+v", res.Alerts[0])
	}
	// t0+5m: fires.
	res = Step(r, res.Alerts, []Sample{sample("shop", 14)}, t0.Add(5*time.Minute))
	if res.Alerts[0].State != StateFiring || len(res.Transitions) != 1 || res.Transitions[0].Event != StateFiring ||
		!res.Alerts[0].FiredAt.Equal(t0.Add(5*time.Minute)) || !res.Alerts[0].StartedAt.Equal(t0) {
		t.Fatalf("t+5m: %+v", res)
	}
	// t0+6m: still firing, no new transition.
	res = Step(r, res.Alerts, []Sample{sample("shop", 15)}, t0.Add(6*time.Minute))
	if res.Alerts[0].State != StateFiring || len(res.Transitions) != 0 || res.Alerts[0].Value != 15 {
		t.Fatalf("t+6m: %+v", res)
	}
	// t0+7m: value drops below threshold → resolved.
	res = Step(r, res.Alerts, []Sample{sample("shop", 9)}, t0.Add(7*time.Minute))
	if res.Alerts[0].State != StateResolved || len(res.Transitions) != 1 || res.Transitions[0].Event != StateResolved {
		t.Fatalf("t+7m: %+v", res)
	}
	resolved := res.Alerts
	// Resolved alerts linger 24h, then are dropped.
	res = Step(r, resolved, nil, t0.Add(7*time.Minute+23*time.Hour))
	if len(res.Alerts) != 1 || len(res.Deleted) != 0 {
		t.Fatalf("kept < 24h: %+v", res)
	}
	res = Step(r, resolved, nil, t0.Add(7*time.Minute+24*time.Hour))
	if len(res.Alerts) != 0 || len(res.Deleted) != 1 {
		t.Fatalf("dropped after 24h: %+v", res)
	}
}

func TestStepPendingThatClearsIsForgotten(t *testing.T) {
	r := stepRule(10 * time.Minute)
	res := Step(r, nil, []Sample{sample("shop", 12)}, t0)
	res = Step(r, res.Alerts, nil, t0.Add(time.Minute)) // series vanished
	if len(res.Alerts) != 0 || len(res.Deleted) != 1 || len(res.Transitions) != 0 {
		t.Fatalf("pending → inactive silently: %+v", res)
	}
}

func TestStepZeroPendingFiresImmediatelyAndRefires(t *testing.T) {
	r := stepRule(0)
	res := Step(r, nil, []Sample{sample("shop", 12)}, t0)
	if res.Alerts[0].State != StateFiring || len(res.Transitions) != 1 {
		t.Fatalf("immediate fire: %+v", res)
	}
	res = Step(r, res.Alerts, nil, t0.Add(time.Minute))
	if res.Alerts[0].State != StateResolved {
		t.Fatal("resolve")
	}
	// A resolved alert whose condition returns starts a fresh cycle.
	res = Step(r, res.Alerts, []Sample{sample("shop", 20)}, t0.Add(2*time.Minute))
	a := res.Alerts[0]
	if a.State != StateFiring || !a.StartedAt.Equal(t0.Add(2*time.Minute)) || !a.ResolvedAt.IsZero() || len(res.Transitions) != 1 {
		t.Fatalf("re-fire: %+v", a)
	}
}

func TestStepSeriesCapAndDuplicates(t *testing.T) {
	r := stepRule(0)
	var many []Sample
	for i := 0; i < MaxSeries+50; i++ {
		many = append(many, Sample{Labels: map[string]string{"i": string(rune('a'+i%26)) + string(rune('a'+i/26%26)) + string(rune('a'+i/676))}, Value: 99})
	}
	many = append(many[:1], many...) // duplicate of the first
	res := Step(r, nil, many, t0)
	if len(res.Alerts) > MaxSeries {
		t.Fatalf("series cap exceeded: %d", len(res.Alerts))
	}
}

func TestFingerprintIgnoresOrder(t *testing.T) {
	a := Fingerprint(map[string]string{"a": "1", "b": "2"})
	b := Fingerprint(map[string]string{"b": "2", "a": "1"})
	c := Fingerprint(map[string]string{"a": "12", "b": ""})
	if a != b || a == c {
		t.Fatal("fingerprint must be order-independent and unambiguous")
	}
}
