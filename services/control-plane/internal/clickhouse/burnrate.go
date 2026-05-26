// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
// We deliberately query the 1s raw table (not the 1m rollup) for the
// short window case (5m–15m); the rollup is faster but its 1-minute
// edge causes flapping near the trigger threshold.
type BurnRateProvider struct {
	DB *sql.DB
}

// MonthlyCeilingUSD is the budget side. Caller (RPC handler) reads the
// matching BudgetPolicy from PostgreSQL and parses the Ceiling string.
type Reading struct {
	BurnRateMilli int32
	Source        string
}

// Compute pulls the actual cost over `window` for the given namespace,
// extrapolates to a monthly figure, and returns
// floor((actual_per_month / ceiling) * 1000). Returns ErrUnavailable
// when no rows exist or the ceiling is non-positive.
func (p *BurnRateProvider) Compute(
	ctx context.Context,
	clusterID, namespace, window string,
	monthlyCeilingUSD float64,
) (Reading, error) {
	if p.DB == nil {
		return Reading{}, ErrUnavailable
	}
	if monthlyCeilingUSD <= 0 {
		return Reading{}, ErrUnavailable
	}

	dur, err := time.ParseDuration(strings.TrimSpace(window))
	if err != nil || dur <= 0 {
		return Reading{}, fmt.Errorf("parse window %q: %w", window, err)
	}
	cutoff := time.Now().UTC().Add(-dur).UnixMilli()

	// SUM(cost_usd_sec) is dollars-per-second over the window. Divide by
	// the window seconds to get dollars/sec at the current rate, then
	// extrapolate to monthly (× 86400 × 30).
	const q = `
		SELECT sum(cost_usd_sec) AS spend_per_sec_total
		FROM pod_cost_1s
		WHERE cluster_id = ? AND namespace = ? AND ts >= ?`

	var totalPerSec sql.NullFloat64
	if err := p.DB.QueryRowContext(ctx, q, clusterID, namespace, cutoff).Scan(&totalPerSec); err != nil {
		return Reading{}, fmt.Errorf("clickhouse burn-rate query: %w", err)
	}
	if !totalPerSec.Valid || totalPerSec.Float64 <= 0 {
		return Reading{}, ErrUnavailable
	}

	// totalPerSec is the SUM of cost_usd_sec rows over `window`; divide
	// by the window's seconds to get the *average* per-second rate.
	avgPerSec := totalPerSec.Float64 / dur.Seconds()
	// Monthly = per-sec × seconds-per-month (≈ 86400 × 30).
	monthly := avgPerSec * 86400 * 30
	ratio := monthly / monthlyCeilingUSD
	milli := int32(ratio * 1000)
	if milli < 0 {
		milli = 0
	}
	return Reading{BurnRateMilli: milli, Source: "clickhouse"}, nil
}
