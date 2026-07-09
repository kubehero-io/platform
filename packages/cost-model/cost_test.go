// SPDX-License-Identifier: Apache-2.0
package costmodel

import "testing"

func TestPodCostPerHour(t *testing.T) {
	node := NodePrice{PerHourUSD: 0.96, CPUMillis: 8000, MemBytes: 32 * 1024 * 1024 * 1024}
	pod := PodShare{CPUMillis: 2000, MemBytes: 8 * 1024 * 1024 * 1024}
	got := PodCostPerHour(node, pod)
	want := 0.96 * 0.25 // pod uses 1/4 of both dimensions
	if got < want-1e-6 || got > want+1e-6 {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestPodCostZeroAllocatable(t *testing.T) {
	if got := PodCostPerHour(NodePrice{PerHourUSD: 1}, PodShare{CPUMillis: 100}); got != 0 {
		t.Fatalf("got %v want 0", got)
	}
}

func TestUtilizationBlend(t *testing.T) {
	gib := int64(1024 * 1024 * 1024)
	cases := []struct {
		name      string
		requested PodShare
		measured  PodShare
		want      PodShare
	}{
		{
			name:      "usage below requests bills requests",
			requested: PodShare{CPUMillis: 2000, MemBytes: 8 * gib},
			measured:  PodShare{CPUMillis: 150, MemBytes: 1 * gib},
			want:      PodShare{CPUMillis: 2000, MemBytes: 8 * gib},
		},
		{
			name:      "usage above requests bills usage",
			requested: PodShare{CPUMillis: 500, MemBytes: 1 * gib},
			measured:  PodShare{CPUMillis: 3200, MemBytes: 5 * gib},
			want:      PodShare{CPUMillis: 3200, MemBytes: 5 * gib},
		},
		{
			name:      "dimensions blend independently",
			requested: PodShare{CPUMillis: 4000, MemBytes: 2 * gib},
			measured:  PodShare{CPUMillis: 900, MemBytes: 6 * gib},
			want:      PodShare{CPUMillis: 4000, MemBytes: 6 * gib},
		},
		{
			name:      "no requests bills pure usage",
			requested: PodShare{},
			measured:  PodShare{CPUMillis: 250, MemBytes: 3 * gib},
			want:      PodShare{CPUMillis: 250, MemBytes: 3 * gib},
		},
		{
			name:      "zero measurement leaves requests untouched",
			requested: PodShare{CPUMillis: 100, MemBytes: 1 * gib},
			measured:  PodShare{},
			want:      PodShare{CPUMillis: 100, MemBytes: 1 * gib},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UtilizationBlend(c.requested, c.measured); got != c.want {
				t.Errorf("UtilizationBlend(%+v, %+v) = %+v, want %+v", c.requested, c.measured, got, c.want)
			}
		})
	}
}
