// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrUnavailable is returned when the burn rate cannot be computed —
// usually because no rows exist for the window yet, or the budget
// reference resolved to a ceiling we couldn't parse. Callers map this
// to the operator's "Tripped=Unknown" condition.
var ErrUnavailable = errors.New("burn rate unavailable")

// BurnRateProvider reads pod_cost_1s within a window and divides by the
// monthly-equivalent ceiling to produce a burn rate × 1000.
//
// We deliberately query the raw table (not the hourly rollup) for the
// short window case (5m–15m); the rollup is faster but its 1-hour edge
// causes flapping near the trigger threshold.
type BurnRateProvider struct {
	DB *sql.DB
	// Now overrides the clock (tests).
	Now func() time.Time
}

// MonthlyCeilingUSD is the budget side. Caller (RPC handler) reads the
// matching BudgetPolicy from PostgreSQL and parses the Ceiling string.
type Reading struct {
	BurnRateMilli int32
	Source        string
}

// secondsPerMonth is the monthly-extrapolation convention shared with
// the anomaly provider: 30 days.
const secondsPerMonth = 86400 * 30

// Compute pulls the actual spend over `window` for the given scope,
// extrapolates it to a monthly figure, and returns
// floor((actual_per_month / ceiling) * 1000). Returns ErrUnavailable
// when no rows exist or the ceiling is non-positive.
//
// Scope: an empty clusterID is fleet-wide and an empty namespace is
// every namespace (GetBurnRateRequest documents "" as fleet-wide).
func (p *BurnRateProvider) Compute(
	ctx context.Context,
	clusterID, namespace, window string,
	monthlyCeilingUSD float64,
) (Reading, error) {
	if p == nil || p.DB == nil {
		return Reading{}, ErrUnavailable
	}
	if monthlyCeilingUSD <= 0 {
		return Reading{}, ErrUnavailable
	}

	dur, err := time.ParseDuration(strings.TrimSpace(window))
	if err != nil || dur <= 0 {
		return Reading{}, fmt.Errorf("parse window %q: %w", window, err)
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	end := now().UTC()
	q, args := burnRateQuery(clusterID, namespace, end.Add(-dur), end)

	var spendUSD sql.NullFloat64
	if err := p.DB.QueryRowContext(ctx, q, args...).Scan(&spendUSD); err != nil {
		return Reading{}, fmt.Errorf("clickhouse burn-rate query: %w", err)
	}
	milli, ok := burnRateMilli(spendUSD.Float64, dur, monthlyCeilingUSD)
	if !spendUSD.Valid || !ok {
		return Reading{}, ErrUnavailable
	}
	return Reading{BurnRateMilli: milli, Source: "clickhouse"}, nil
}

// burnRateQuery prices the window's rows. Collectors sample every few
// seconds, so a row's dollars are its per-second rate times the
// seconds it covers — sum(cost_usd_sec * interval_sec) — not
// sum(cost_usd_sec), which undercounted 5s samples five-fold.
func burnRateQuery(clusterID, namespace string, start, end time.Time) (string, []any) {
	var b strings.Builder
	b.WriteString(`SELECT sum(cost_usd_sec * interval_sec) AS spend_usd
		FROM pod_cost_1s
		WHERE ts >= ? AND ts < ?`)
	args := []any{start.UnixMilli(), end.UnixMilli()}
	if clusterID != "" {
		b.WriteString(` AND cluster_id = ?`)
		args = append(args, clusterID)
	}
	if namespace != "" {
		b.WriteString(` AND namespace = ?`)
		args = append(args, namespace)
	}
	return b.String(), args
}

// burnRateMilli turns the window's spend into the burn rate × 1000:
// spend / window seconds is the average $/s, × 30 days is the monthly
// run rate, ÷ the ceiling is the multiple. ok is false when there was
// no spend to extrapolate.
func burnRateMilli(spendUSD float64, window time.Duration, monthlyCeilingUSD float64) (int32, bool) {
	if spendUSD <= 0 || window <= 0 || monthlyCeilingUSD <= 0 {
		return 0, false
	}
	monthly := spendUSD / window.Seconds() * secondsPerMonth
	// Floor, but absorb floating-point noise from summing thousands of
	// samples (60 × $0.05 is 2.9999999999999996) so spending exactly at
	// a threshold reads as that threshold.
	milli := math.Floor(monthly/monthlyCeilingUSD*1000 + 1e-6)
	if milli > math.MaxInt32 {
		return math.MaxInt32, true
	}
	return int32(milli), true
}
