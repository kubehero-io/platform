// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package metrics

import (
	"strings"
	"sync"
	"testing"
)

func TestRegistryExposition(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("t_items_total", "Items.", "signal", "reason")
	g := r.NewGaugeVec("t_depth", "Depth\nwith newline.")
	c.With("logs", "queue_full").Add(3)
	c.With("cost", `quote"back\slash`).Inc()
	g.With().Set(7)
	if r.NewCounterVec("t_items_total", "again", "signal", "reason") != c {
		t.Fatal("re-registration must return the existing family")
	}
	var sb strings.Builder
	r.Write(&sb)
	want := "# HELP t_items_total Items.\n# TYPE t_items_total counter\n" +
		"t_items_total{signal=\"cost\",reason=\"quote\\\"back\\\\slash\"} 1\n" +
		"t_items_total{signal=\"logs\",reason=\"queue_full\"} 3\n" +
		"# HELP t_depth Depth\\nwith newline.\n# TYPE t_depth gauge\nt_depth 7\n"
	if sb.String() != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", sb.String(), want)
	}
}

func TestRegistryConcurrentAdds(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounterVec("t_concurrent_total", "x", "k")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				c.With("a").Add(0.5)
			}
		}()
	}
	wg.Wait()
	if got := c.With("a").Get(); got != 4000 {
		t.Fatalf("got %v, want 4000", got)
	}
}

func TestRegistryShapeMismatchPanics(t *testing.T) {
	r := NewRegistry()
	r.NewCounterVec("t_shape", "x", "a")
	defer func() {
		if recover() == nil {
			t.Fatal("re-registering with other labels must panic")
		}
	}()
	r.NewGaugeVec("t_shape", "x", "b")
}

func TestPublishEBPF(t *testing.T) {
	PublishEBPF(map[string]bool{"netflow": true, "profiler": false}, map[string]uint64{"flows_emitted": 42})
	if EBPFAttached.With("netflow").Get() != 1 || EBPFAttached.With("profiler").Get() != 0 || EBPFCounters.With("flows_emitted").Get() != 42 {
		t.Fatal("eBPF snapshot not published")
	}
}

func TestWriteAllGroupsFamilies(t *testing.T) {
	var sb strings.Builder
	WriteAll(&sb, []Series{
		{Name: "b", Value: 1}, {Name: "a", Labels: map[string]string{"z": "1", "y": "2"}, Value: 2}, {Name: "b", Value: 3},
	})
	if sb.String() != "a{y=\"2\",z=\"1\"} 2\nb 1\nb 3\n" {
		t.Fatalf("got %q", sb.String())
	}
}
