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

const gib = int64(1024 * 1024 * 1024)

func approx(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= 1e-9
}

// On CPU-only nodes the breakdown must be exactly PodCostPerHour split
// in two — the GPU-aware model may not move a single cent of existing
// CPU-only spend.
func TestPodCostBreakdownCPUOnlyMatchesPodCostPerHour(t *testing.T) {
	node := NodePrice{PerHourUSD: 0.768, CPUMillis: 15_890, MemBytes: 62 * gib}
	cases := []struct {
		name string
		pod  PodShare
	}{
		{"balanced", PodShare{CPUMillis: 2000, MemBytes: 8 * gib}},
		{"cpu heavy", PodShare{CPUMillis: 12_000, MemBytes: 1 * gib}},
		{"memory heavy", PodShare{CPUMillis: 100, MemBytes: 48 * gib}},
		{"best effort", PodShare{}},
		{"gpu request on cpu node is ignored", PodShare{CPUMillis: 500, MemBytes: gib, GPUs: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cpu, ram, gpu := PodCostBreakdown(node, c.pod)
			if gpu != 0 {
				t.Fatalf("gpu part on a CPU-only node = %v, want 0", gpu)
			}
			if want := PodCostPerHour(node, c.pod); !approx(cpu+ram, want) {
				t.Fatalf("cpu+ram = %v, PodCostPerHour = %v", cpu+ram, want)
			}
			// Each half prices its own dimension only.
			if want := node.PerHourUSD / 2 * float64(c.pod.CPUMillis) / float64(node.CPUMillis); !approx(cpu, want) {
				t.Errorf("cpu = %v, want %v", cpu, want)
			}
		})
	}
}

func TestPodCostBreakdownGPUNode(t *testing.T) {
	// 8×GPU node at $10/h with 96 cores and 1 TiB allocatable:
	// $7/h of accelerators ($0.875 per GPU), $1.5/h CPU, $1.5/h RAM.
	node := NodePrice{PerHourUSD: 10, CPUMillis: 96_000, MemBytes: 1024 * gib, GPUs: 8}
	cases := []struct {
		name                string
		pod                 PodShare
		wantCPU, wantRAM, w float64
	}{
		{
			// 1/8 of everything → exactly 1/8 of the price.
			name:    "one eighth of every dimension",
			pod:     PodShare{CPUMillis: 12_000, MemBytes: 128 * gib, GPUs: 1},
			wantCPU: 0.1875, wantRAM: 0.1875, w: 0.875,
		},
		{
			// A sidecar on a GPU node pays CPU/RAM out of the 30% pool only.
			name:    "no gpu requested",
			pod:     PodShare{CPUMillis: 1000, MemBytes: 4 * gib},
			wantCPU: 1.5 / 96, wantRAM: 1.5 * 4 / 1024, w: 0,
		},
		{
			name:    "two gpus, little cpu",
			pod:     PodShare{CPUMillis: 4000, MemBytes: 32 * gib, GPUs: 2},
			wantCPU: 1.5 * 4 / 96, wantRAM: 1.5 * 32 / 1024, w: 1.75,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cpu, ram, gpu := PodCostBreakdown(node, c.pod)
			if !approx(cpu, c.wantCPU) || !approx(ram, c.wantRAM) || !approx(gpu, c.w) {
				t.Fatalf("breakdown = (%v, %v, %v), want (%v, %v, %v)", cpu, ram, gpu, c.wantCPU, c.wantRAM, c.w)
			}
		})
	}
}

// Pods that together request the whole node pay the whole price; any
// unrequested capacity is the node's idle cost, never double-billed.
func TestPodCostBreakdownFullAllocationSumsToPrice(t *testing.T) {
	for _, node := range []NodePrice{
		{PerHourUSD: 32.77, CPUMillis: 96_000, MemBytes: 1152 * gib, GPUs: 8},
		{PerHourUSD: 0.192, CPUMillis: 3_920, MemBytes: 15 * gib},
	} {
		n := int64(8)
		total := 0.0
		for range n {
			cpu, ram, gpu := PodCostBreakdown(node, PodShare{
				CPUMillis: node.CPUMillis / n,
				MemBytes:  node.MemBytes / n,
				GPUs:      node.GPUs / n,
			})
			total += cpu + ram + gpu
		}
		if !approx(total, node.PerHourUSD) {
			t.Errorf("node %+v: 8 pods × 1/8 sum to %v, want %v", node, total, node.PerHourUSD)
		}
	}
}

func TestPodCostBreakdownZeroAllocatable(t *testing.T) {
	cpu, ram, gpu := PodCostBreakdown(NodePrice{PerHourUSD: 1}, PodShare{CPUMillis: 100, MemBytes: gib})
	if cpu != 0 || ram != 0 || gpu != 0 {
		t.Fatalf("unsized CPU node must price at 0, got (%v, %v, %v)", cpu, ram, gpu)
	}
	// GPU attribution only depends on GPU counts, so it survives a node
	// whose CPU/memory allocatable wasn't reported.
	cpu, ram, gpu = PodCostBreakdown(NodePrice{PerHourUSD: 8, GPUs: 4}, PodShare{CPUMillis: 100, GPUs: 1})
	if cpu != 0 || ram != 0 || !approx(gpu, 8*GPUPriceShare/4) {
		t.Fatalf("unsized GPU node = (%v, %v, %v), want (0, 0, %v)", cpu, ram, gpu, 8*GPUPriceShare/4)
	}
}

func TestHourlyToPerSecondMatchesPodCostPerSecond(t *testing.T) {
	node := NodePrice{PerHourUSD: 0.96, CPUMillis: 8000, MemBytes: 32 * gib}
	pod := PodShare{CPUMillis: 2000, MemBytes: 8 * gib}
	if got, want := HourlyToPerSecond(PodCostPerHour(node, pod)), PodCostPerSecond(node, pod); got != want {
		t.Fatalf("HourlyToPerSecond = %v, PodCostPerSecond = %v", got, want)
	}
}

func TestUnusedShare(t *testing.T) {
	cases := []struct {
		name                string
		requested, measured PodShare
		want                PodShare
	}{
		{"idle reservation", PodShare{CPUMillis: 2000, MemBytes: 8 * gib, GPUs: 1}, PodShare{CPUMillis: 150, MemBytes: gib}, PodShare{CPUMillis: 1850, MemBytes: 7 * gib}},
		{"burst floors at zero", PodShare{CPUMillis: 500, MemBytes: gib}, PodShare{CPUMillis: 3200, MemBytes: 5 * gib}, PodShare{}},
		{"mixed", PodShare{CPUMillis: 4000, MemBytes: 2 * gib}, PodShare{CPUMillis: 900, MemBytes: 6 * gib}, PodShare{CPUMillis: 3100}},
		{"no requests", PodShare{}, PodShare{CPUMillis: 10, MemBytes: 10}, PodShare{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UnusedShare(c.requested, c.measured); got != c.want {
				t.Errorf("UnusedShare = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestUtilizationBlendKeepsGPURequest(t *testing.T) {
	got := UtilizationBlend(PodShare{CPUMillis: 100, GPUs: 2}, PodShare{CPUMillis: 50})
	if got.GPUs != 2 {
		t.Fatalf("GPU request must stay billable, got %+v", got)
	}
}
