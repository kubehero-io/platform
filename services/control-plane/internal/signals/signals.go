// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package signals holds the small set of types shared between the
// control plane's signal engines — the logs engine (LogQL), and the
// consumers that evaluate its metric queries (the alert evaluator,
// the advisor-facing RPCs). Keeping them here lets each engine live in
// its own package without import cycles.
package signals

import (
	"context"
	"time"
)

// Point is one sample of a time series.
type Point struct {
	TS    time.Time
	Value float64
}

// Series is a labelled time series.
type Series struct {
	Labels map[string]string
	Points []Point
}

// Last returns the most recent point's value, or false when empty.
func (s Series) Last() (float64, bool) {
	if len(s.Points) == 0 {
		return 0, false
	}
	return s.Points[len(s.Points)-1].Value, true
}

// LogMetricQuerier evaluates a LogQL metric query (count_over_time,
// rate, bytes_over_time, … with optional sum/avg/min/max/topk by (…))
// over [start, end] at the given step. Implemented by the logs engine;
// consumed by the alert evaluator for kind=logs rules.
type LogMetricQuerier interface {
	QueryMetric(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error)
}
