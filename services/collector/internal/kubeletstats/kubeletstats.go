// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package kubeletstats reads measured utilisation from the kubelet
// Summary API (/stats/summary): node totals, per-pod totals and
// per-container usage. Two transports are supported:
//
//   - API-server node proxy (default):
//     GET /api/v1/nodes/{node}/proxy/stats/summary
//     Works everywhere with `get nodes/proxy` in the ClusterRole, but
//     every byte transits the API server — fine for small clusters, a
//     real load on large ones.
//   - Direct kubelet (DaemonSet mode, --kubelet-url=https://$NODE_IP:10250):
//     the collector on each node asks its own kubelet with its service
//     account token. Needs `get nodes/stats` (the kubelet authorises
//     /stats/* as that subresource via SubjectAccessReview) and, unless
//     kubelet serving certs are signed by the cluster CA
//     (serverTLSBootstrap), --kubelet-insecure-tls — the same trade-off
//     metrics-server documents.
//
// The response is the kubelet's statsapi Summary; we decode only the
// cpu.usageNanoCores and memory.workingSetBytes fields at node, pod and
// container level, and deliberately avoid importing k8s.io/kubelet just
// for its types.
//
// The kubelet refreshes these numbers on its housekeeping interval
// (~10–15s), so Cached shares one fetch between the cost scanner (5s)
// and the usage sampler (30s) instead of asking twice as often as the
// data changes.
package kubeletstats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// PodKey identifies a pod within a cluster.
type PodKey struct {
	Namespace string
	Name      string
}

// Usage is the measured utilisation snapshot for one pod at the time
// the kubelet compiled its summary.
type Usage struct {
	// CPUMillicores is cpu.usageNanoCores converted to millicores
	// (1 core = 1e9 nanocores = 1000 millicores).
	CPUMillicores int64
	// MemoryWorkingSetBytes is memory.workingSetBytes — the number the
	// OOM killer cares about, not RSS.
	MemoryWorkingSetBytes int64
}

// ContainerUsage is one container's measured utilisation.
type ContainerUsage struct {
	// CPUNanoCores keeps full precision for fractional-core reporting
	// (ContainerUsage.cpu_usage_cores on the wire).
	CPUNanoCores          uint64
	MemoryWorkingSetBytes uint64
	// HasCPU / HasMemory are false when the kubelet hasn't sampled that
	// dimension yet (a container that just started) — callers should
	// not mistake "unknown" for "idle".
	HasCPU    bool
	HasMemory bool
}

// PodStats is one pod's totals plus its containers, keyed by name.
type PodStats struct {
	UID        string
	Usage      Usage
	Containers map[string]ContainerUsage
}

// NodeStats is the node-level (whole machine) usage.
type NodeStats struct {
	Name                  string
	CPUMillicores         int64
	MemoryWorkingSetBytes int64
	HasCPU                bool
	HasMemory             bool
}

// Summary is the parsed subset of a kubelet /stats/summary response.
type Summary struct {
	Node NodeStats
	Pods map[PodKey]PodStats
}

// PodUsage flattens the summary to per-pod usage, the shape the cost
// scanner consumes.
func (s *Summary) PodUsage() map[PodKey]Usage {
	out := make(map[PodKey]Usage, len(s.Pods))
	for k, p := range s.Pods {
		out[k] = p.Usage
	}
	return out
}

// Provider is the surface the collector consumes. It is an interface so
// tests can substitute a fixture-backed fake for the live kubelet.
type Provider interface {
	// Summary returns the parsed summary of the kubelet on `node`.
	Summary(ctx context.Context, node string) (*Summary, error)
}

// maxSummaryBytes bounds what we are willing to buffer from a kubelet.
// A 250-pod node with volumes and per-interface network stats is well
// under 4 MiB; 32 MiB leaves headroom without letting a misbehaving
// endpoint balloon the agent.
const maxSummaryBytes = 32 << 20

// ── API-server node proxy ─────────────────────────────────────────────

// Client fetches summaries through the API-server node proxy.
type Client struct {
	rest rest.Interface
}

var _ Provider = (*Client)(nil)

// New builds a Client on top of an existing clientset, sharing its
// transport, auth and rate limiting.
func New(k8s kubernetes.Interface) *Client {
	return &Client{rest: k8s.CoreV1().RESTClient()}
}

// Summary implements Provider against the API server.
func (c *Client) Summary(ctx context.Context, node string) (*Summary, error) {
	raw, err := c.rest.Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats/summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("stats/summary via node proxy: %w", err)
	}
	return Parse(raw)
}

// ── direct kubelet ────────────────────────────────────────────────────

// DirectConfig configures Direct.
type DirectConfig struct {
	// URL is the kubelet base URL, e.g. https://10.0.1.23:10250.
	URL string
	// TokenFile is re-read as the projected token rotates (client-go's
	// bearer-token-file transport handles the refresh).
	TokenFile string
	// CAFile verifies the kubelet serving cert; ignored when Insecure.
	CAFile   string
	Insecure bool
	Timeout  time.Duration
}

// Direct fetches summaries straight from one kubelet. The node argument
// of Summary is ignored: a Direct provider only ever talks to the
// kubelet it was configured for (the one on the collector's own node).
type Direct struct {
	url  string
	http *http.Client
}

var _ Provider = (*Direct)(nil)

// NewDirect builds a Direct provider.
func NewDirect(cfg DirectConfig) (*Direct, error) {
	if cfg.URL == "" {
		return nil, errors.New("kubelet url is empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	rc := &rest.Config{
		Host:            cfg.URL,
		BearerTokenFile: cfg.TokenFile,
		Timeout:         cfg.Timeout,
		TLSClientConfig: rest.TLSClientConfig{Insecure: cfg.Insecure},
	}
	if !cfg.Insecure {
		rc.TLSClientConfig.CAFile = cfg.CAFile
	}
	hc, err := rest.HTTPClientFor(rc)
	if err != nil {
		return nil, fmt.Errorf("kubelet http client: %w", err)
	}
	return &Direct{url: strings.TrimRight(cfg.URL, "/") + "/stats/summary", http: hc}, nil
}

// Summary implements Provider against the local kubelet.
func (d *Direct) Summary(ctx context.Context, _ string) (*Summary, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stats/summary from kubelet: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("stats/summary from kubelet: http %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxSummaryBytes+1))
	if err != nil {
		return nil, fmt.Errorf("stats/summary from kubelet: %w", err)
	}
	if len(raw) > maxSummaryBytes {
		return nil, fmt.Errorf("stats/summary from kubelet: response exceeds %d bytes", maxSummaryBytes)
	}
	return Parse(raw)
}

// ── caching ───────────────────────────────────────────────────────────

// Cached wraps a Provider with a per-node TTL so concurrent consumers
// share one fetch. Errors are not cached: the next caller retries.
type Cached struct {
	inner Provider
	ttl   time.Duration
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*cacheEntry
}

type cacheEntry struct {
	at       time.Time
	summary  *Summary
	inflight chan struct{} // non-nil while a fetch is running
	err      error         // result of the last fetch, for waiters
}

var _ Provider = (*Cached)(nil)

// NewCached returns a caching Provider. ttl <= 0 disables caching.
func NewCached(inner Provider, ttl time.Duration) *Cached {
	return &Cached{inner: inner, ttl: ttl, now: time.Now, entries: map[string]*cacheEntry{}}
}

// Summary implements Provider.
func (c *Cached) Summary(ctx context.Context, node string) (*Summary, error) {
	c.mu.Lock()
	e := c.entries[node]
	if e == nil {
		e = &cacheEntry{}
		c.entries[node] = e
	}
	if e.summary != nil && c.ttl > 0 && c.now().Sub(e.at) < c.ttl {
		s := e.summary
		c.mu.Unlock()
		return s, nil
	}
	if ch := e.inflight; ch != nil {
		// Someone is already fetching this node; wait for them.
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		s, err := e.summary, e.err
		c.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	ch := make(chan struct{})
	e.inflight = ch
	c.mu.Unlock()

	s, err := c.inner.Summary(ctx, node)

	c.mu.Lock()
	e.inflight = nil
	e.err = err
	if err == nil {
		e.summary, e.at = s, c.now()
	}
	c.mu.Unlock()
	close(ch)
	return s, err
}

// ── wire shapes ────────────────────────────────────────────────────────
// A hand-rolled subset of k8s.io/kubelet/pkg/apis/stats/v1alpha1 — the
// full module is a heavy dependency for a handful of numbers.

type summaryWire struct {
	Node struct {
		NodeName string      `json:"nodeName"`
		CPU      *cpuWire    `json:"cpu"`
		Memory   *memoryWire `json:"memory"`
	} `json:"node"`
	Pods []podWire `json:"pods"`
}

type cpuWire struct {
	UsageNanoCores *uint64 `json:"usageNanoCores"`
}

type memoryWire struct {
	WorkingSetBytes *uint64 `json:"workingSetBytes"`
}

type podWire struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		UID       string `json:"uid"`
	} `json:"podRef"`
	Containers []struct {
		Name   string      `json:"name"`
		CPU    *cpuWire    `json:"cpu"`
		Memory *memoryWire `json:"memory"`
	} `json:"containers"`
	CPU    *cpuWire    `json:"cpu"`
	Memory *memoryWire `json:"memory"`
}

func (c *cpuWire) nanoCores() (uint64, bool) {
	if c == nil || c.UsageNanoCores == nil {
		return 0, false
	}
	return *c.UsageNanoCores, true
}

func (m *memoryWire) workingSet() (uint64, bool) {
	if m == nil || m.WorkingSetBytes == nil {
		return 0, false
	}
	return *m.WorkingSetBytes, true
}

// clampInt64 converts kubelet counters (uint64) without wrapping
// negative on absurd values.
func clampInt64(v uint64) int64 {
	if v > 1<<62 {
		return 1 << 62
	}
	return int64(v)
}

// Parse decodes a raw /stats/summary payload. Unknown fields (volumes,
// network, filesystem, system containers…) are ignored. Pods missing a
// podRef name are skipped; pods or containers missing a cpu or memory
// block (kubelet hasn't sampled them yet) report zero for that
// dimension with the Has* flag cleared rather than being dropped.
func Parse(raw []byte) (*Summary, error) {
	var s summaryWire
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode stats/summary: %w", err)
	}
	out := &Summary{Pods: make(map[PodKey]PodStats, len(s.Pods))}
	out.Node.Name = s.Node.NodeName
	if v, ok := s.Node.CPU.nanoCores(); ok {
		out.Node.CPUMillicores, out.Node.HasCPU = clampInt64(v/1_000_000), true
	}
	if v, ok := s.Node.Memory.workingSet(); ok {
		out.Node.MemoryWorkingSetBytes, out.Node.HasMemory = clampInt64(v), true
	}
	for _, p := range s.Pods {
		if p.PodRef.Name == "" {
			continue
		}
		ps := PodStats{UID: p.PodRef.UID}
		if v, ok := p.CPU.nanoCores(); ok {
			ps.Usage.CPUMillicores = clampInt64(v / 1_000_000)
		}
		if v, ok := p.Memory.workingSet(); ok {
			ps.Usage.MemoryWorkingSetBytes = clampInt64(v)
		}
		if len(p.Containers) > 0 {
			ps.Containers = make(map[string]ContainerUsage, len(p.Containers))
			for _, c := range p.Containers {
				if c.Name == "" {
					continue
				}
				var cu ContainerUsage
				cu.CPUNanoCores, cu.HasCPU = c.CPU.nanoCores()
				cu.MemoryWorkingSetBytes, cu.HasMemory = c.Memory.workingSet()
				ps.Containers[c.Name] = cu
			}
		}
		out.Pods[PodKey{Namespace: p.PodRef.Namespace, Name: p.PodRef.Name}] = ps
	}
	return out, nil
}

// ParseSummary decodes a raw /stats/summary payload into per-pod usage.
// It is Parse flattened to pod totals.
func ParseSummary(raw []byte) (map[PodKey]Usage, error) {
	s, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return s.PodUsage(), nil
}
