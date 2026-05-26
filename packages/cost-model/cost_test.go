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
