// SPDX-License-Identifier: BUSL-1.1
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// fake cost query — deterministic numbers per (clusterID, window)
type fakeCost struct {
	burn map[string]float64
	mtd  map[string]float64
}

func (f *fakeCost) BurnRateRatio(_ context.Context, cluster string, _ time.Duration) (float64, error) {
	if f.burn == nil {
		return 0, nil
	}
	v, ok := f.burn[cluster]
	if !ok {
		return 0, nil
	}
	return v, nil
}
func (f *fakeCost) MonthToDateSpendUSD(_ context.Context, cluster string) (float64, error) {
	if f.mtd == nil {
		return 0, errors.New("no data")
	}
	return f.mtd[cluster], nil
}

func spec(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestArmingGate(t *testing.T) {
	e := &Evaluator{Cost: &fakeCost{}}
	p := &store.Policy{
		ID: "p1", Kind: "BudgetPolicy", ClusterID: "c1",
		SpecJSON: spec(t, map[string]any{
			"ceiling":  "$100000/mo",
			"hardStop": true,
			// humanArm omitted — defaults true
		}),
	}
	d, err := e.Evaluate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Trigger != "requires-arm" {
		t.Fatalf("trigger %q want requires-arm", d.Trigger)
	}
}

func TestCeilingBurnRateFires(t *testing.T) {
	e := &Evaluator{Cost: &fakeCost{burn: map[string]float64{"c1": 2.4}}}
	p := &store.Policy{
		ID: "p2", Kind: "CeilingPolicy", ClusterID: "c1", Armed: true,
		SpecJSON: spec(t, map[string]any{
			"trigger":  map[string]any{"burnRateMilli": 2000, "window": "5m"},
			"humanArm": false,
		}),
	}
	d, err := e.Evaluate(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !d.ShouldFire {
		t.Fatalf("expected fire at 2.4x vs 2.0x threshold; got %+v", d)
	}
}

func TestBudgetBelowThreshold(t *testing.T) {
	e := &Evaluator{Cost: &fakeCost{mtd: map[string]float64{"c1": 5_000}}}
	p := &store.Policy{
		ID: "p3", Kind: "BudgetPolicy", ClusterID: "c1", Armed: true,
		SpecJSON: spec(t, map[string]any{
			"ceiling":  "$100000/mo",
			"hardStop": true,
			"humanArm": false,
		}),
	}
	d, _ := e.Evaluate(context.Background(), p)
	if d.Trigger != "below-threshold" {
		t.Fatalf("trigger %q · reason %q", d.Trigger, d.Reason)
	}
}

func TestBudgetExceeded(t *testing.T) {
	e := &Evaluator{Cost: &fakeCost{mtd: map[string]float64{"c1": 110_000}}}
	p := &store.Policy{
		ID: "p4", Kind: "BudgetPolicy", ClusterID: "c1", Armed: true,
		SpecJSON: spec(t, map[string]any{
			"ceiling":  "$100000/mo",
			"hardStop": true,
			"humanArm": false,
		}),
	}
	d, _ := e.Evaluate(context.Background(), p)
	if d.Trigger != "budget-exceeded" || !d.ShouldFire {
		t.Fatalf("expected budget-exceeded + fire; got %+v", d)
	}
}

func TestCeilingHumanArmAbsentDefaultsArmed(t *testing.T) {
	e := &Evaluator{Cost: &fakeCost{burn: map[string]float64{"c1": 3.0}}}
	p := &store.Policy{
		ID: "p5", Kind: "CeilingPolicy", ClusterID: "c1", Armed: false,
		SpecJSON: spec(t, map[string]any{
			"trigger": map[string]any{"burnRateMilli": 1500, "window": "1m"},
			// humanArm omitted
		}),
	}
	d, _ := e.Evaluate(context.Background(), p)
	if d.Trigger != "requires-arm" {
		t.Fatalf("expected requires-arm; got %+v", d)
	}
}

func TestParseCeiling(t *testing.T) {
	cases := []struct {
		in  string
		out float64
	}{
		{"$100000/mo", 100_000},
		{"300/hr", 300 * 24 * 30},
		{"$1,500", 1500},
		{"garbage", 0},
	}
	for _, c := range cases {
		got := parseCeiling(c.in)
		if got != c.out {
			t.Errorf("parseCeiling(%q) = %v want %v", c.in, got, c.out)
		}
	}
}
