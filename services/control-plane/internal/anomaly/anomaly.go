// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package anomaly holds the pure statistical core of KubeHero's
// spend-anomaly detection: rolling z-scores of a "current" bucket
// against a trailing baseline. It deliberately knows nothing about
// ClickHouse, protobuf, or Kubernetes — callers assemble the series,
// this package scores it. That keeps the math trivially unit-testable
// and reusable by the operator once client-side detection lands.
package anomaly

import "math"

const (
	// DefaultZThreshold is the |z| at which a series is flagged
	// anomalous when the caller doesn't configure one. 3σ ≈ 0.3% false
	// positive rate on a normal series — conservative enough that a
	// card on the overview page means something.
	DefaultZThreshold = 3.0

	// DefaultMinBaseline is the minimum number of baseline buckets
	// required before a z-score is considered meaningful. Below this,
	// mean/stddev are too noisy to trust and Score reports the series
	// as unscoreable rather than guessing.
	DefaultMinBaseline = 6
)

// Stats is the z-score verdict for one series.
type Stats struct {
	// Mean and StdDev are the population statistics of the baseline.
	Mean   float64
	StdDev float64
	// Z is (current − mean) / stddev. Positive = spend spike,
	// negative = spend drop.
	Z float64
	// DeltaPct is the signed percent change of current vs the baseline
	// mean, e.g. +34 for a 34% spike. Zero when the mean is zero.
	DeltaPct float64
}

// Anomalous reports whether the score crosses the given |z| threshold.
// A non-positive threshold falls back to DefaultZThreshold.
func (s Stats) Anomalous(threshold float64) bool {
	if threshold <= 0 {
		threshold = DefaultZThreshold
	}
	return math.Abs(s.Z) >= threshold
}

// Score computes the rolling z-score of `current` against `baseline`.
//
// The boolean result is false — and Stats is zero — when the series
// cannot be scored:
//
//   - insufficient data: fewer than minBaseline buckets (minBaseline
//     <= 0 falls back to DefaultMinBaseline);
//   - zero variance: a perfectly flat baseline has stddev 0, and
//     dividing by it would produce ±Inf. Real cost series always
//     carry noise, so a flat one means synthetic or missing data —
//     we refuse to score it rather than fabricate an infinite z.
func Score(baseline []float64, current float64, minBaseline int) (Stats, bool) {
	if minBaseline <= 0 {
		minBaseline = DefaultMinBaseline
	}
	if len(baseline) < minBaseline {
		return Stats{}, false
	}

	mean := meanOf(baseline)
	variance := 0.0
	for _, v := range baseline {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(baseline))
	stddev := math.Sqrt(variance)
	if stddev == 0 {
		return Stats{}, false
	}

	s := Stats{
		Mean:   mean,
		StdDev: stddev,
		Z:      (current - mean) / stddev,
	}
	if mean != 0 {
		s.DeltaPct = (current - mean) / mean * 100
	}
	return s, true
}

func meanOf(vs []float64) float64 {
	sum := 0.0
	for _, v := range vs {
		sum += v
	}
	return sum / float64(len(vs))
}
