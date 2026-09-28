// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package usage samples per-container utilisation against requests and
// limits — the history the control plane's percentile rightsizing reads
// (container_usage → 5-minute t-digest rollups).
//
// Numbers come from the kubelet summary (cpu.usageNanoCores,
// memory.workingSetBytes) joined with the pod spec and container
// status. A container the kubelet hasn't sampled yet is skipped rather
// than reported as zero: a zero would drag the low percentiles down and
// make the recommender undersize.
package usage

import (
	"context"
	"log/slog"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// Inventory is what the sampler reads from the kube cache.
type Inventory interface {
	Pods() []*corev1.Pod
	Node(name string) *corev1.Node
	Nodes() []*corev1.Node
}

// Owners resolves pods to workloads.
type Owners interface {
	Resolve(ctx context.Context, p *corev1.Pod) kube.Workload
}

// Config configures the sampler.
type Config struct {
	ClusterID string
	NodeName  string // "" = every node (dev)
	Interval  time.Duration
	Logger    *slog.Logger
}

// Sampler emits one IngestUsageRequest per interval.
type Sampler struct {
	cfg    Config
	inv    Inventory
	owners Owners
	stats  kubeletstats.Provider
	emit   func(*kuberov1.IngestUsageRequest)
	warned map[string]bool
}

// New builds a Sampler.
func New(cfg Config, inv Inventory, owners Owners, stats kubeletstats.Provider, emit func(*kuberov1.IngestUsageRequest)) *Sampler {
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Sampler{cfg: cfg, inv: inv, owners: owners, stats: stats, emit: emit, warned: map[string]bool{}}
}

// Run samples every interval until ctx ends.
func (s *Sampler) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		start := time.Now()
		if req := s.Sample(ctx, start); req != nil && s.emit != nil {
			s.emit(req)
		}
		metrics.ScanDuration.With("usage").Set(time.Since(start).Seconds())
	}
}

// Sample builds one request (nil when there is nothing to report).
func (s *Sampler) Sample(ctx context.Context, now time.Time) *kuberov1.IngestUsageRequest {
	byNode := map[string][]*corev1.Pod{}
	for _, p := range s.inv.Pods() {
		if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
			continue
		}
		if s.cfg.NodeName != "" && p.Spec.NodeName != s.cfg.NodeName {
			continue
		}
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}
	nodes := make([]string, 0, len(byNode))
	for n := range byNode {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	req := &kuberov1.IngestUsageRequest{ClusterId: s.cfg.ClusterID}
	for _, nodeName := range nodes {
		sum, err := s.stats.Summary(ctx, nodeName)
		if err != nil {
			metrics.ScanErrors.With("usage", "kubelet_stats").Inc()
			if !s.warned[nodeName] {
				s.warned[nodeName] = true
				s.cfg.Logger.Warn("kubelet stats unavailable — skipping container usage for this node", "node", nodeName, "err", err)
			}
			continue
		}
		delete(s.warned, nodeName)
		node := s.inv.Node(nodeName)
		for _, p := range byNode[nodeName] {
			ps, ok := sum.Pods[kubeletstats.PodKey{Namespace: p.Namespace, Name: p.Name}]
			if !ok || (ps.UID != "" && !kube.MatchesUID(p, ps.UID)) {
				continue
			}
			req.Usage = append(req.Usage, s.podRows(ctx, now, p, node, ps)...)
		}
	}
	if len(req.Usage) == 0 {
		return nil
	}
	return req
}

func (s *Sampler) podRows(ctx context.Context, now time.Time, p *corev1.Pod, node *corev1.Node, ps kubeletstats.PodStats) []*kuberov1.ContainerUsage {
	statuses := map[string]*corev1.ContainerStatus{}
	for i := range p.Status.ContainerStatuses {
		statuses[p.Status.ContainerStatuses[i].Name] = &p.Status.ContainerStatuses[i]
	}
	for i := range p.Status.InitContainerStatuses {
		statuses[p.Status.InitContainerStatuses[i].Name] = &p.Status.InitContainerStatuses[i]
	}
	// App containers plus native sidecars — the containers that run for
	// the pod's lifetime and whose requests can be rightsized.
	containers := make([]*corev1.Container, 0, len(p.Spec.Containers)+len(p.Spec.InitContainers))
	for i := range p.Spec.Containers {
		containers = append(containers, &p.Spec.Containers[i])
	}
	for i := range p.Spec.InitContainers {
		c := &p.Spec.InitContainers[i]
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			containers = append(containers, c)
		}
	}

	var w kube.Workload
	var out []*kuberov1.ContainerUsage
	for _, c := range containers {
		cu, ok := ps.Containers[c.Name]
		if !ok || !cu.HasCPU || !cu.HasMemory {
			continue
		}
		if w.Kind == "" {
			w = kube.Workload{Name: p.Name, Kind: "Pod"}
			if s.owners != nil {
				w = s.owners.Resolve(ctx, p)
			}
		}
		ref := kube.PodRefFor(p, node, w)
		ref.Container = c.Name
		req, lim := kube.ContainerRequests(c), kube.ContainerLimits(c)
		row := &kuberov1.ContainerUsage{
			TsUnixMs:           now.UnixMilli(),
			Source:             ref,
			CpuUsageCores:      float64(cu.CPUNanoCores) / 1e9,
			MemWorkingSetBytes: cu.MemoryWorkingSetBytes,
			CpuRequestCores:    float64(req.CPUMillis) / 1000,
			MemRequestBytes:    uint64(req.MemBytes),
			CpuLimitCores:      float64(lim.CPUMillis) / 1000,
			MemLimitBytes:      uint64(lim.MemBytes),
		}
		if st := statuses[c.Name]; st != nil {
			row.Restarts = uint32(max(st.RestartCount, 0))
			if t := st.LastTerminationState.Terminated; t != nil {
				row.LastTerminationReason = t.Reason
			}
		}
		out = append(out, row)
	}
	return out
}
