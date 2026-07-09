// SPDX-License-Identifier: BUSL-1.1
package anomaly

import (
	"math"
	"testing"
)

// flat returns n copies of v — a convenience for baselines.
func flat(n int, v float64) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestScore(t *testing.T) {
	// noisy24 is a plausible hourly-spend baseline: ~$10/h with small
	// jitter, mean 10.0, stddev ≈ 0.7.
	noisy24 := []float64{
		10.2, 9.8, 10.5, 9.5, 10.0, 10.3, 9.7, 10.1,
		9.9, 10.4, 9.6, 10.0, 10.2, 9.8, 10.1, 9.9,
		10.6, 9.4, 10.0, 10.3, 9.7, 10.1, 9.9, 10.0,
	}

	cases := []struct {
		name        string
		baseline    []float64
		current     float64
		minBaseline int

		wantOK        bool
		wantAnomalous bool // evaluated at the default 3.0 threshold
		wantZSign     int  // -1, 0, +1 — sign of the expected z
	}{
		{
			name:     "spike detection",
			baseline: noisy24, current: 40.0,
			wantOK: true, wantAnomalous: true, wantZSign: 1,
		},
		{
			name:     "drop detection",
			baseline: noisy24, current: 2.0,
			wantOK: true, wantAnomalous: true, wantZSign: -1,
		},
		{
			name:     "quiet series stays quiet",
			baseline: noisy24, current: 10.1,
			wantOK: true, wantAnomalous: false, wantZSign: 1,
		},
		{
			name:     "mild bump below threshold",
			baseline: noisy24, current: 10.6,
			wantOK: true, wantAnomalous: false, wantZSign: 1,
		},
		{
			name:     "zero-variance guard",
			baseline: flat(24, 10.0), current: 100.0,
			wantOK: false,
		},
		{
			name:     "all-zero baseline is zero variance too",
			baseline: flat(24, 0), current: 50.0,
			wantOK: false,
		},
		{
			name:     "insufficient data",
			baseline: []float64{10, 12, 11}, current: 99.0,
			wantOK: false,
		},
		{
			name:     "empty baseline",
			baseline: nil, current: 5.0,
			wantOK: false,
		},
		{
			name:     "custom minBaseline accepts short series",
			baseline: []float64{10, 12, 11, 13}, current: 30.0,
			minBaseline: 4,
			wantOK:      true, wantAnomalous: true, wantZSign: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, ok := Score(c.baseline, c.current, c.minBaseline)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v (stats=%+v)", ok, c.wantOK, s)
			}
			if !ok {
				if s != (Stats{}) {
					t.Fatalf("unscoreable series must return zero Stats, got %+v", s)
				}
				return
			}
			if math.IsNaN(s.Z) || math.IsInf(s.Z, 0) {
				t.Fatalf("z must be finite, got %v", s.Z)
			}
			if got := s.Anomalous(0); got != c.wantAnomalous {
				t.Fatalf("Anomalous(default)=%v want %v (z=%.2f)", got, c.wantAnomalous, s.Z)
			}
			if c.wantZSign > 0 && s.Z <= 0 || c.wantZSign < 0 && s.Z >= 0 {
				t.Fatalf("z sign: got %.2f want sign %d", s.Z, c.wantZSign)
			}
			// DeltaPct must agree in sign with z when the mean is positive.
			if s.Mean > 0 && (s.DeltaPct > 0) != (s.Z > 0) {
				t.Fatalf("delta pct %v disagrees with z %v", s.DeltaPct, s.Z)
			}
		})
	}
}

func TestScoreStatsValues(t *testing.T) {
	// Hand-computable baseline: mean 10, population stddev 2.
	baseline := []float64{8, 12, 8, 12, 8, 12}
	s, ok := Score(baseline, 16, 6)
	if !ok {
		t.Fatal("expected scoreable series")
	}
	if s.Mean != 10 {
		t.Errorf("mean=%v want 10", s.Mean)
	}
	if s.StdDev != 2 {
		t.Errorf("stddev=%v want 2", s.StdDev)
	}
	if s.Z != 3 {
		t.Errorf("z=%v want 3", s.Z)
	}
	if s.DeltaPct != 60 {
		t.Errorf("delta=%v want 60", s.DeltaPct)
	}
	if !s.Anomalous(3.0) {
		t.Error("z=3 must be anomalous at threshold 3")
	}
	if s.Anomalous(3.5) {
		t.Error("z=3 must not be anomalous at threshold 3.5")
	}
}

func TestAnomalousThresholdFallback(t *testing.T) {
	s := Stats{Z: -3.2}
	if !s.Anomalous(-1) {
		t.Error("non-positive threshold must fall back to default and flag |z|=3.2")
	}
	if !s.Anomalous(0) {
		t.Error("zero threshold must fall back to default")
	}
}
