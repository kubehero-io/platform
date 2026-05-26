// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package engine evaluates BudgetPolicy + CeilingPolicy objects against
// live cost rollups and decides whether their escalation plan should
// invoke. It never executes enforcement itself — that's the runner
// package's job — but it owns the burn-rate math and the humanArm gate.

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

// Decision is the evaluator's output: what (if anything) should happen.
type Decision struct {
	PolicyID   string
	Trigger    string // "armed-ok", "requires-arm", "below-threshold", "budget-exceeded", "burn-rate-2x", ...
	ShouldFire bool
	Reason     string
	At         time.Time
}

// CostQuery is the narrow interface the evaluator needs from ClickHouse.
// Implemented by a real CH-backed adapter in production; swap for a
// fake in tests.
type CostQuery interface {
	// BurnRateRatio returns spend rate over `window` divided by the
	// reference rate. A value of 2.0 means the current rate is 2x
	// the reference. Returns 0 when no data is available.
	BurnRateRatio(ctx context.Context, clusterID string, window time.Duration) (float64, error)
	// MonthToDateSpendUSD returns the current month's accumulated spend
	// for the given scope (empty cluster = fleet-wide).
	MonthToDateSpendUSD(ctx context.Context, clusterID string) (float64, error)
}

// Evaluator wires the stores + cost query backend.
type Evaluator struct {
	Policies store.PolicyStore
	Cost     CostQuery
}

func New(p store.PolicyStore, q CostQuery) *Evaluator {
	return &Evaluator{Policies: p, Cost: q}
}

// Evaluate returns a Decision for the given policy. Does not persist
// anything — callers store via PolicyStore.RecordEval + AuditStore.Append.
func (e *Evaluator) Evaluate(ctx context.Context, p *store.Policy) (*Decision, error) {
	d := &Decision{PolicyID: p.ID, At: time.Now().UTC()}

	// Arming gate: humanArm defaults true; escalation is gated unless armed.
	needsArm, err := needsArming(p.SpecJSON)
	if err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	if needsArm && !p.Armed {
		d.Trigger = "requires-arm"
		d.Reason = "spec.humanArm=true · awaiting kubehero cap --arm"
		return d, nil
	}

	switch p.Kind {
	case "BudgetPolicy":
		return e.evalBudget(ctx, p, d)
	case "CeilingPolicy":
		return e.evalCeiling(ctx, p, d)
	default:
		d.Trigger = "no-op"
		return d, nil
	}
}

func (e *Evaluator) evalBudget(ctx context.Context, p *store.Policy, d *Decision) (*Decision, error) {
	var spec struct {
		Ceiling  string `json:"ceiling"`
		HardStop bool   `json:"hardStop"`
	}
	if err := json.Unmarshal(p.SpecJSON, &spec); err != nil {
		return d, err
	}
	ceilingUSD := parseCeiling(spec.Ceiling)
	if ceilingUSD <= 0 {
		d.Trigger = "below-threshold"
		d.Reason = "ceiling unparseable or zero"
		return d, nil
	}
	mtd, err := e.Cost.MonthToDateSpendUSD(ctx, p.ClusterID)
	if err != nil {
		return d, err
	}
	pct := (mtd / ceilingUSD) * 100
	switch {
	case pct >= 100:
		d.Trigger = "budget-exceeded"
		d.ShouldFire = spec.HardStop
		d.Reason = fmt.Sprintf("month-to-date %.0f%% of ceiling", pct)
	case pct >= 95:
		d.Trigger = "threshold-95"
		d.Reason = fmt.Sprintf("%.1f%% of ceiling", pct)
	case pct >= 80:
		d.Trigger = "threshold-80"
		d.Reason = fmt.Sprintf("%.1f%% of ceiling", pct)
	default:
		d.Trigger = "below-threshold"
		d.Reason = fmt.Sprintf("%.1f%% of ceiling", pct)
	}
	return d, nil
}

func (e *Evaluator) evalCeiling(ctx context.Context, p *store.Policy, d *Decision) (*Decision, error) {
	var spec struct {
		BudgetRef string `json:"budgetRef"`
		Trigger   struct {
			BurnRateMilli int32  `json:"burnRateMilli"`
			Window        string `json:"window"`
		} `json:"trigger"`
	}
	if err := json.Unmarshal(p.SpecJSON, &spec); err != nil {
		return d, err
	}
	window, err := time.ParseDuration(spec.Trigger.Window)
	if err != nil || window <= 0 {
		window = 5 * time.Minute
	}
	ratio, err := e.Cost.BurnRateRatio(ctx, p.ClusterID, window)
	if err != nil {
		return d, err
	}
	threshold := float64(spec.Trigger.BurnRateMilli) / 1000.0
	if threshold <= 0 {
		threshold = 1.5
	}
	if ratio >= threshold {
		d.Trigger = fmt.Sprintf("burn-rate-%.1fx", ratio)
		d.ShouldFire = true
		d.Reason = fmt.Sprintf("observed %.2fx · threshold %.2fx · window %s", ratio, threshold, window)
	} else {
		d.Trigger = "below-threshold"
		d.Reason = fmt.Sprintf("observed %.2fx · threshold %.2fx", ratio, threshold)
	}
	return d, nil
}
