// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package kubeletstats

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return raw
}

func TestParseSummaryFixture(t *testing.T) {
	got, err := ParseSummary(readFixture(t, "stats_summary.json"))
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

func TestParseNodeAndContainers(t *testing.T) {
	s, err := Parse(readFixture(t, "stats_summary_containers.json"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Node.Name != "gke-prod-pool-1-a1b2" || !s.Node.HasCPU || !s.Node.HasMemory {
		t.Fatalf("node stats = %+v", s.Node)
	}
	// 1750500000 nanocores → 1750 millicores (truncated, not rounded).
	if s.Node.CPUMillicores != 1750 || s.Node.MemoryWorkingSetBytes != 7516192768 {
		t.Fatalf("node usage = %+v", s.Node)
	}
	if len(s.Pods) != 1 {
		t.Fatalf("nameless pod must be skipped, got %d pods", len(s.Pods))
	}
	p := s.Pods[PodKey{Namespace: "shop", Name: "checkout-7d9f8b6c5-x2x4q"}]
	if p.UID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("uid = %q", p.UID)
	}
	if p.Usage.CPUMillicores != 262 || p.Usage.MemoryWorkingSetBytes != 318767104 {
		t.Fatalf("pod usage = %+v", p.Usage)
	}
	cases := []struct {
		name   string
		want   ContainerUsage
		exists bool
	}{
		{"app", ContainerUsage{CPUNanoCores: 250500000, MemoryWorkingSetBytes: 268435456, HasCPU: true, HasMemory: true}, true},
		{"istio-proxy", ContainerUsage{CPUNanoCores: 12000000, MemoryWorkingSetBytes: 50331648, HasCPU: true, HasMemory: true}, true},
		// null cpu, no memory block: present but flagged unknown.
		{"warming-up", ContainerUsage{}, true},
		{"", ContainerUsage{}, false},
	}
	for _, c := range cases {
		got, ok := p.Containers[c.name]
		if ok != c.exists {
			t.Errorf("container %q present=%v, want %v", c.name, ok, c.exists)
			continue
		}
		if got != c.want {
			t.Errorf("container %q = %+v, want %+v", c.name, got, c.want)
		}
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
	s, _ := Parse([]byte(`{"node":{"nodeName":"n1"}}`))
	if s.Node.HasCPU || s.Node.HasMemory {
		t.Fatalf("missing node blocks must be flagged unknown: %+v", s.Node)
	}
}

// The API-server proxy path must hit exactly the node proxy subresource.
func TestClientUsesNodeProxy(t *testing.T) {
	raw := readFixture(t, "stats_summary_containers.json")
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cs).Summary(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if p, _ := gotPath.Load().(string); p != "/api/v1/nodes/node-a/proxy/stats/summary" {
		t.Fatalf("path = %q", p)
	}
	if s.Node.Name != "gke-prod-pool-1-a1b2" {
		t.Fatalf("summary not parsed: %+v", s.Node)
	}
}

func TestDirectSendsBearerAndParses(t *testing.T) {
	raw := readFixture(t, "stats_summary_containers.json")
	var auth, path atomic.Value
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		path.Store(r.URL.Path)
		_, _ = w.Write(raw)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("sa-token-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := NewDirect(DirectConfig{URL: srv.URL + "/", TokenFile: tok, Insecure: true})
	if err != nil {
		t.Fatal(err)
	}
	s, err := d.Summary(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got, _ := auth.Load().(string); got != "Bearer sa-token-1" {
		t.Fatalf("Authorization = %q", got)
	}
	if got, _ := path.Load().(string); got != "/stats/summary" {
		t.Fatalf("path = %q", got)
	}
	if len(s.Pods) != 1 {
		t.Fatalf("pods = %d", len(s.Pods))
	}
}

func TestDirectRejectsErrorsAndOversize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	d, err := NewDirect(DirectConfig{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Summary(context.Background(), ""); err == nil {
		t.Fatal("expected error for http 403")
	}
	if _, err := NewDirect(DirectConfig{}); err == nil {
		t.Fatal("expected error for empty url")
	}
}

// ── Cached ─────────────────────────────────────────────────────────────

type countingProvider struct {
	calls atomic.Int32
	delay time.Duration
	err   error
}

func (c *countingProvider) Summary(ctx context.Context, node string) (*Summary, error) {
	c.calls.Add(1)
	if c.delay > 0 {
		time.Sleep(c.delay)
	}
	if c.err != nil {
		return nil, c.err
	}
	return &Summary{Node: NodeStats{Name: node}}, nil
}

func TestCachedServesWithinTTL(t *testing.T) {
	inner := &countingProvider{}
	c := NewCached(inner, 10*time.Second)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }

	for range 3 {
		if _, err := c.Summary(context.Background(), "n1"); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls.Load() != 1 {
		t.Fatalf("calls within TTL = %d, want 1", inner.calls.Load())
	}
	// A different node is a different entry.
	_, _ = c.Summary(context.Background(), "n2")
	if inner.calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", inner.calls.Load())
	}
	now = now.Add(11 * time.Second)
	_, _ = c.Summary(context.Background(), "n1")
	if inner.calls.Load() != 3 {
		t.Fatalf("calls after expiry = %d, want 3", inner.calls.Load())
	}
}

func TestCachedDoesNotCacheErrors(t *testing.T) {
	inner := &countingProvider{err: errors.New("kubelet down")}
	c := NewCached(inner, time.Minute)
	for range 2 {
		if _, err := c.Summary(context.Background(), "n1"); err == nil {
			t.Fatal("expected error")
		}
	}
	if inner.calls.Load() != 2 {
		t.Fatalf("errors must not be cached: calls = %d", inner.calls.Load())
	}
}

func TestCachedCoalescesConcurrentFetches(t *testing.T) {
	inner := &countingProvider{delay: 50 * time.Millisecond}
	c := NewCached(inner, time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s, err := c.Summary(context.Background(), "n1"); err != nil || s.Node.Name != "n1" {
				t.Errorf("Summary = %v, %v", s, err)
			}
		}()
	}
	wg.Wait()
	if inner.calls.Load() != 1 {
		t.Fatalf("concurrent callers must share one fetch, got %d", inner.calls.Load())
	}
}
