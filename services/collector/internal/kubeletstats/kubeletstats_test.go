// SPDX-License-Identifier: Apache-2.0
package kubeletstats

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseSummaryFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "stats_summary.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := ParseSummary(raw)
	if err != nil {
		t.Fatalf("ParseSummary: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("expected 4 pods, got %d: %v", len(got), got)
	}

	cases := []struct {
		name    string
		key     PodKey
		wantCPU int64
		wantMem int64
	}{
		{
			// Fully-sampled pod: 3423000000 nanocores → 3423 millicores.
			name:    "gpu model server",
			key:     PodKey{Namespace: "ml", Name: "model-server-a100-0"},
			wantCPU: 3423,
			wantMem: 18253611008,
		},
		{
			name:    "small api pod",
			key:     PodKey{Namespace: "edge", Name: "api-1"},
			wantCPU: 41,
			wantMem: 73400320,
		},
		{
			// usageNanoCores is null (kubelet hasn't sampled cpu yet) —
			// cpu degrades to 0, memory still reported.
			name:    "just-started pod with null cpu",
			key:     PodKey{Namespace: "jobs", Name: "warmup-batch-7dk2p"},
			wantCPU: 0,
			wantMem: 524288,
		},
		{
			// Sandbox created but no cpu/memory blocks at all.
			name:    "sandbox-only pod",
			key:     PodKey{Namespace: "jobs", Name: "sandbox-only-x9f4b"},
			wantCPU: 0,
			wantMem: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, ok := got[c.key]
			if !ok {
				t.Fatalf("pod %v missing from parse result", c.key)
			}
			if u.CPUMillicores != c.wantCPU {
				t.Errorf("cpu millicores = %d, want %d", u.CPUMillicores, c.wantCPU)
			}
			if u.MemoryWorkingSetBytes != c.wantMem {
				t.Errorf("memory working set = %d, want %d", u.MemoryWorkingSetBytes, c.wantMem)
			}
		})
	}
}

func TestParseSummaryRejectsGarbage(t *testing.T) {
	if _, err := ParseSummary([]byte("not json")); err == nil {
		t.Fatal("expected error for non-JSON payload")
	}
}

func TestParseSummaryEmptyPods(t *testing.T) {
	got, err := ParseSummary([]byte(`{"node":{"nodeName":"n1"}}`))
	if err != nil {
		t.Fatalf("ParseSummary: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %v", got)
	}
}
