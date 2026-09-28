// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package clickhouse

import (
	"strings"
	"testing"
	"time"
)

func TestBurnRateMilli(t *testing.T) {
	tests := []struct {
		name    string
		spend   float64
		window  time.Duration
		ceiling float64
		want    int32
		ok      bool
	}{
		// $0.01/s for 5 minutes = $3 spent; 3/300 × 2 592 000 = $25 920/mo
		// against a $12 960 ceiling → 2.0×.
		{"exactly 2x", 3, 5 * time.Minute, 12960, 2000, true},
		{"on budget", 3, 5 * time.Minute, 25920, 1000, true},
		{"no spend", 0, time.Hour, 100, 0, false},
		{"no ceiling", 1, time.Hour, 0, 0, false},
		{"saturates", 1e12, time.Second, 1, 1<<31 - 1, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := burnRateMilli(tc.spend, tc.window, tc.ceiling)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("burnRateMilli = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The regression this fixes: a 5s sample at $0.01/s is $0.05 of spend.
// Summing the rate alone reported one fifth of the real burn.
func TestBurnRatePricesRateTimesInterval(t *testing.T) {
	q, _ := burnRateQuery("c1", "shop", time.Unix(0, 0), time.Unix(300, 0))
	if !strings.Contains(q, "sum(cost_usd_sec * interval_sec)") {
		t.Fatalf("burn-rate query must price rows as rate × interval:\n%s", q)
	}
	// 60 samples of 5s at $0.01/s over a 5 minute window = $3 → with a
	// $12 960/mo ceiling that is 2.0×; the pre-fix sum(rate)=0.6 gave 0.4×.
	spend := 0.0
	for i := 0; i < 60; i++ {
		spend += 0.01 * 5
	}
	if got, _ := burnRateMilli(spend, 5*time.Minute, 12960); got != 2000 {
		t.Fatalf("burn rate = %d‰, want 2000‰", got)
	}
}

func TestBurnRateQueryScope(t *testing.T) {
	start, end := time.UnixMilli(1000), time.UnixMilli(2000)
	tests := []struct {
		cluster, ns string
		wantSQL     []string
		notSQL      []string
		wantArgs    int
	}{
		{"c1", "shop", []string{"cluster_id = ?", "namespace = ?"}, nil, 4},
		{"", "shop", []string{"namespace = ?"}, []string{"cluster_id"}, 3},
		{"c1", "", []string{"cluster_id = ?"}, []string{"namespace"}, 3},
		{"", "", nil, []string{"cluster_id", "namespace ="}, 2},
	}
	for _, tc := range tests {
		q, args := burnRateQuery(tc.cluster, tc.ns, start, end)
		for _, s := range tc.wantSQL {
			if !strings.Contains(q, s) {
				t.Errorf("(%q,%q): query lacks %q", tc.cluster, tc.ns, s)
			}
		}
		for _, s := range tc.notSQL {
			if strings.Contains(q, s) {
				t.Errorf("(%q,%q): query should not filter %q", tc.cluster, tc.ns, s)
			}
		}
		if len(args) != tc.wantArgs || args[0] != int64(1000) || args[1] != int64(2000) {
			t.Errorf("(%q,%q): args = %v", tc.cluster, tc.ns, args)
		}
	}
}
