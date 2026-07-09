// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package kubeletstats reads measured pod utilisation from the kubelet
// Summary API. Rather than talking to every kubelet directly (which
// would need the node's serving cert plus a host-network path), we go
// through the API-server node proxy:
//
//	GET /api/v1/nodes/{node}/proxy/stats/summary
//
// which requires `get` on the `nodes/proxy` resource in the collector's
// ClusterRole. The response is the kubelet's statsapi Summary; we decode
// only the pod-level cpu.usageNanoCores and memory.workingSetBytes
// fields, and deliberately avoid importing k8s.io/kubelet just for its
// types.
package kubeletstats

import (
	"context"
	"encoding/json"
	"fmt"

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

// Provider is the surface the scanner consumes. It is an interface so
// tests can substitute a fixture-backed fake for the live API server.
type Provider interface {
	// PodUsage returns measured usage for every pod the kubelet on
	// `node` currently reports, keyed by namespace/name. Pods without
	// a cpu or memory reading yet (e.g. just started) report 0 for
	// that dimension.
	PodUsage(ctx context.Context, node string) (map[PodKey]Usage, error)
}

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

// PodUsage implements Provider against the live API server.
func (c *Client) PodUsage(ctx context.Context, node string) (map[PodKey]Usage, error) {
	raw, err := c.rest.Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats/summary").
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("stats/summary via node proxy: %w", err)
	}
	return ParseSummary(raw)
}

// ── wire shapes ────────────────────────────────────────────────────────
// A hand-rolled subset of k8s.io/kubelet/pkg/apis/stats/v1alpha1 — the
// full module is a heavy dependency for two numbers per pod.

type summary struct {
	Pods []podStats `json:"pods"`
}

type podStats struct {
	PodRef struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"podRef"`
	CPU *struct {
		UsageNanoCores *uint64 `json:"usageNanoCores"`
	} `json:"cpu"`
	Memory *struct {
		WorkingSetBytes *uint64 `json:"workingSetBytes"`
	} `json:"memory"`
}

// ParseSummary decodes a raw /stats/summary payload into per-pod usage.
// Unknown fields (node stats, per-container breakdowns, volumes…) are
// ignored. Pods missing a podRef name are skipped; pods missing a cpu
// or memory block (kubelet hasn't sampled them yet) get a zero for that
// dimension rather than being dropped.
func ParseSummary(raw []byte) (map[PodKey]Usage, error) {
	var s summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode stats/summary: %w", err)
	}
	out := make(map[PodKey]Usage, len(s.Pods))
	for _, p := range s.Pods {
		if p.PodRef.Name == "" {
			continue
		}
		var u Usage
		if p.CPU != nil && p.CPU.UsageNanoCores != nil {
			u.CPUMillicores = int64(*p.CPU.UsageNanoCores / 1_000_000)
		}
		if p.Memory != nil && p.Memory.WorkingSetBytes != nil {
			u.MemoryWorkingSetBytes = int64(*p.Memory.WorkingSetBytes)
		}
		out[PodKey{Namespace: p.PodRef.Namespace, Name: p.PodRef.Name}] = u
	}
	return out, nil
}
