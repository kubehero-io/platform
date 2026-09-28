// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package ingest is the collector's cost pipeline. Every scan (5s by
// default) it prices the running pods on THIS node and the node itself,
// and hands one IngestPodCostRequest to the ship queue.
//
// Architectural notes:
//
//   - Node locality is the correctness rule. The collector runs as a
//     DaemonSet; each instance reports only the pods bound to its own
//     node (a spec.nodeName-scoped informer, see internal/kube) and
//     fetches only its own kubelet summary. A collector that reported
//     every pod would make an N-node cluster report N× its real cost.
//     Only when NODE_NAME is unset (local development against a
//     kubeconfig) does one scanner price every node — main.go warns
//     loudly, because that mode double-counts if run as a DaemonSet.
//
//   - A pod's billable share is max(requests, measured usage) per
//     dimension (packages/cost-model.UtilizationBlend): requests floor
//     the bill because the scheduler reserved that capacity, measured
//     usage above requests bills burstable overage. When the kubelet
//     summary is unavailable we degrade to requests — never crash.
//
//   - The $ math is packages/cost-model, split CPU / RAM / GPU, so the
//     collector, control plane and CLI price a pod identically. The
//     node's hourly price comes from the kubehero.io/node-hourly-usd
//     annotation, else the pricing engine (1h cache), else an estimate
//     (see pricing.go), and is recorded with its source.
//
//   - Recoverable cost is the priced unused reservation:
//     (requested − measured, floored at 0) per dimension through the
//     same CPU/RAM rates, i.e. what rightsizing requests down to observed
//     usage would free. It is only emitted with a measurement; GPUs are
//     excluded without accelerator utilisation telemetry.
//
//   - Spend accounting is rate × interval: each sample carries the wall
//     seconds since that pod's previous sample (first sample: the scan
//     interval, or less for a pod that started within it), so a slow or
//     stalled scan neither loses nor double-counts time. Intervals are
//     capped at maxInterval to keep a clock jump or a long process
//     freeze from inventing spend.
//
//   - Idle cost is the node's price minus what its pods were billed,
//     floored at 0 — the denominator the control plane shares out.
package ingest

import (
	"context"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	costmodel "github.com/kubehero-io/platform/packages/cost-model"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

const (
	// maxInterval bounds how much wall time one sample may claim.
	maxInterval = 5 * time.Minute
	// labelResend re-sends a pod's labels even when unchanged, so
	// pod_metadata stays fresh (it has a TTL) and a batch lost during an
	// outage doesn't lose a pod's labels for its whole life.
	labelResend = 6 * time.Hour
	// maxLabels bounds the labels map per sample.
	maxLabels = 64
)

// Inventory is what the scanner reads from the kube cache.
type Inventory interface {
	Pods() []*corev1.Pod
	Node(name string) *corev1.Node
	Nodes() []*corev1.Node
}

// Owners resolves pods to workloads.
type Owners interface {
	Resolve(ctx context.Context, p *corev1.Pod) kube.Workload
}

// Config carries the scanner's knobs.
type Config struct {
	ClusterID string
	// NodeName scopes the scan to one node; "" prices every node (dev).
	NodeName string
	// Interval between scans. Defaults to 5s.
	Interval time.Duration
	Logger   *slog.Logger
}

// Scanner prices pods and nodes.
type Scanner struct {
	cfg    Config
	inv    Inventory
	owners Owners
	stats  kubeletstats.Provider
	pricer *Pricer
	emit   func(*kuberov1.IngestPodCostRequest)
	log    *slog.Logger

	lastPod     map[string]time.Time // pod UID → last sample
	lastNode    map[string]time.Time // node name → last sample
	labelsSent  map[types.UID]labelState
	warnedNodes map[string]bool

	series atomic.Pointer[[]metrics.Series]
}

type labelState struct {
	hash   uint64
	sentAt time.Time
}

// New builds a Scanner. emit receives one request per scan (nil: the
// scan result is only logged — a collector without a control plane).
func New(cfg Config, inv Inventory, owners Owners, stats kubeletstats.Provider, pricer *Pricer, emit func(*kuberov1.IngestPodCostRequest)) *Scanner {
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if pricer == nil {
		pricer = NewPricer(nil, cfg.Logger)
	}
	return &Scanner{
		cfg: cfg, inv: inv, owners: owners, stats: stats, pricer: pricer, emit: emit,
		log:         cfg.Logger,
		lastPod:     map[string]time.Time{},
		lastNode:    map[string]time.Time{},
		labelsSent:  map[types.UID]labelState{},
		warnedNodes: map[string]bool{},
	}
}

// Run scans immediately and then every interval until ctx ends.
func (s *Scanner) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Scanner) tick(ctx context.Context) {
	start := time.Now()
	req := s.Scan(ctx, start)
	metrics.ScanDuration.With("cost").Set(time.Since(start).Seconds())
	if req == nil {
		return
	}
	if s.emit == nil {
		s.log.Debug("cost scan complete (no control plane wired)", "pods", len(req.Samples), "nodes", len(req.Nodes))
		return
	}
	s.emit(req)
}

// Series returns the chargeback series of the last scan for /metrics.
func (s *Scanner) Series() []metrics.Series {
	if p := s.series.Load(); p != nil {
		return *p
	}
	return nil
}

// Scan prices one tick. It returns nil when there is nothing to report
// (e.g. the node object isn't cached yet).
func (s *Scanner) Scan(ctx context.Context, now time.Time) *kuberov1.IngestPodCostRequest {
	byNode := map[string][]*corev1.Pod{}
	for _, p := range s.inv.Pods() {
		if p.Status.Phase != corev1.PodRunning || p.Spec.NodeName == "" {
			continue
		}
		// Defence in depth: the informer is node-scoped, but a pod is
		// only ever priced by the collector on its own node.
		if s.cfg.NodeName != "" && p.Spec.NodeName != s.cfg.NodeName {
			continue
		}
		byNode[p.Spec.NodeName] = append(byNode[p.Spec.NodeName], p)
	}

	var nodes []*corev1.Node
	if s.cfg.NodeName != "" {
		n := s.inv.Node(s.cfg.NodeName)
		if n == nil {
			metrics.ScanErrors.With("cost", "node_unknown").Inc()
			s.warnNode(s.cfg.NodeName, "node object not in cache yet — skipping cost scan", nil)
			return nil
		}
		nodes = []*corev1.Node{n}
	} else {
		nodes = s.inv.Nodes()
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })

	req := &kuberov1.IngestPodCostRequest{ClusterId: s.cfg.ClusterID}
	var series []metrics.Series
	seen := make(map[types.UID]bool, len(s.lastPod))
	seenNodes := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		pods := byNode[n.Name]
		sort.Slice(pods, func(i, j int) bool {
			if pods[i].Namespace != pods[j].Namespace {
				return pods[i].Namespace < pods[j].Namespace
			}
			return pods[i].Name < pods[j].Name
		})
		summary := s.summary(ctx, n.Name)
		perHour, source := s.pricer.Price(ctx, n)
		np := costmodel.NodePrice{
			PerHourUSD: perHour,
			CPUMillis:  kube.AllocatableCPUMillis(n),
			MemBytes:   kube.AllocatableMemBytes(n),
			GPUs:       kube.NodeGPUs(n),
		}
		pl := placementOf(n)

		var sumCost float64
		var reqCPU, reqMem int64
		for _, p := range pods {
			seen[p.UID] = true
			sample, requested := s.podSample(ctx, now, p, np, pl, summary)
			sumCost += sample.CostUsdSec
			reqCPU += requested.CPUMillis
			reqMem += requested.MemBytes
			req.Samples = append(req.Samples, sample)
			series = append(series, podSeries(s.cfg.ClusterID, sample)...)
		}

		seenNodes[n.Name] = true
		ns := &kuberov1.NodeCostSample{
			Node:                     n.Name,
			TsUnixMs:                 now.UnixMilli(),
			Nodepool:                 pl.nodepool,
			Region:                   pl.region,
			Zone:                     pl.zone,
			Sku:                      pl.sku,
			Lifecycle:                pl.lifecycle,
			Cloud:                    pl.cloud,
			PricePerHour:             perHour,
			CpuAllocatableMillicores: clampU32(np.CPUMillis),
			MemAllocatableBytes:      clampU64(np.MemBytes),
			CpuRequestedMillicores:   clampU32(reqCPU),
			MemRequestedBytes:        clampU64(reqMem),
			GpuCount:                 clampU32(np.GPUs),
			GpuKind:                  pl.gpuKind,
			CostUsdSec:               costmodel.HourlyToPerSecond(perHour),
			PriceSource:              source,
			IntervalSec:              float32(s.interval(s.lastNode, n.Name, now, time.Time{}).Seconds()),
		}
		if summary != nil {
			if summary.Node.HasCPU {
				ns.CpuUsedMillicores = clampU32(summary.Node.CPUMillicores)
			}
			if summary.Node.HasMemory {
				ns.MemUsedBytes = clampU64(summary.Node.MemoryWorkingSetBytes)
			}
		}
		ns.IdleUsdSec = math.Max(0, ns.CostUsdSec-sumCost)
		req.Nodes = append(req.Nodes, ns)
		series = append(series, nodeSeries(s.cfg.ClusterID, ns))
	}

	// Forget pods and nodes that are gone so state stays bounded by
	// what the node actually runs.
	for uid := range s.lastPod {
		if !seen[types.UID(uid)] {
			delete(s.lastPod, uid)
			delete(s.labelsSent, types.UID(uid))
		}
	}
	for name := range s.lastNode {
		if !seenNodes[name] {
			delete(s.lastNode, name)
		}
	}
	metrics.PodsObserved.With().Set(float64(len(req.Samples)))
	s.series.Store(&series)
	if len(req.Samples) == 0 && len(req.Nodes) == 0 {
		return nil
	}
	return req
}

// summary fetches the node's kubelet stats. A failing node logs a warn
// once, then its pods degrade to request-based cost until stats
// recover — this path must never take the DaemonSet down.
func (s *Scanner) summary(ctx context.Context, node string) *kubeletstats.Summary {
	if s.stats == nil {
		return nil
	}
	sum, err := s.stats.Summary(ctx, node)
	if err != nil {
		metrics.ScanErrors.With("cost", "kubelet_stats").Inc()
		s.warnNode(node, "kubelet stats unavailable — falling back to request-based cost", err)
		return nil
	}
	if s.warnedNodes[node] {
		delete(s.warnedNodes, node)
		s.log.Info("kubelet stats recovered", "node", node)
	}
	return sum
}

func (s *Scanner) warnNode(node, msg string, err error) {
	if s.warnedNodes[node] {
		return
	}
	s.warnedNodes[node] = true
	if err != nil {
		s.log.Warn(msg, "node", node, "err", err)
	} else {
		s.log.Warn(msg, "node", node)
	}
}

type placement struct {
	nodepool, region, zone, sku, lifecycle, cloud, gpuKind string
}

func placementOf(n *corev1.Node) placement {
	return placement{
		nodepool:  kube.Nodepool(n),
		region:    kube.Region(n),
		zone:      kube.Zone(n),
		sku:       kube.SKU(n),
		lifecycle: kube.Lifecycle(n),
		cloud:     kube.Cloud(n),
		gpuKind:   kube.GPUKind(n),
	}
}

func (s *Scanner) podSample(ctx context.Context, now time.Time, p *corev1.Pod, np costmodel.NodePrice, pl placement, sum *kubeletstats.Summary) (*kuberov1.PodCostSample, costmodel.PodShare) {
	r := kube.PodRequests(p)
	requested := costmodel.PodShare{CPUMillis: r.CPUMillis, MemBytes: r.MemBytes, GPUs: r.GPUs}

	var measured costmodel.PodShare
	measuredOK := false
	if sum != nil {
		// The UID check rejects stats of a previous pod with the same
		// name; static pods are reported under their config hash.
		if ps, ok := sum.Pods[kubeletstats.PodKey{Namespace: p.Namespace, Name: p.Name}]; ok && (ps.UID == "" || kube.MatchesUID(p, ps.UID)) {
			measured = costmodel.PodShare{CPUMillis: ps.Usage.CPUMillicores, MemBytes: ps.Usage.MemoryWorkingSetBytes}
			measuredOK = true
		}
	}
	billable := requested
	if measuredOK {
		billable = costmodel.UtilizationBlend(requested, measured)
	}
	cpuH, ramH, gpuH := costmodel.PodCostBreakdown(np, billable)
	cpuS, ramS, gpuS := costmodel.HourlyToPerSecond(cpuH), costmodel.HourlyToPerSecond(ramH), costmodel.HourlyToPerSecond(gpuH)
	cost := cpuS + ramS + gpuS

	var recoverable float64
	if measuredOK {
		uc, ur, _ := costmodel.PodCostBreakdown(np, costmodel.UnusedShare(requested, measured))
		recoverable = math.Min(costmodel.HourlyToPerSecond(uc+ur), cost)
	}

	var start time.Time
	if p.Status.StartTime != nil {
		start = p.Status.StartTime.Time
	}
	w := kube.Workload{Name: p.Name, Kind: "Pod"}
	if s.owners != nil {
		w = s.owners.Resolve(ctx, p)
	}
	sample := &kuberov1.PodCostSample{
		Cluster:            s.cfg.ClusterID,
		Namespace:          p.Namespace,
		Pod:                p.Name,
		TsUnixMs:           now.UnixMilli(),
		Team:               kube.Team(p),
		CostCenter:         kube.CostCenter(p),
		Nodepool:           pl.nodepool,
		Node:               p.Spec.NodeName,
		Region:             pl.region,
		Sku:                pl.sku,
		Lifecycle:          pl.lifecycle,
		CpuMillicores:      clampU32(requested.CPUMillis),
		MemBytes:           clampU64(requested.MemBytes),
		CostUsdSec:         cost,
		RecoverableUsdSec:  recoverable,
		CpuCostUsdSec:      cpuS,
		RamCostUsdSec:      ramS,
		GpuCostUsdSec:      gpuS,
		Workload:           w.Name,
		WorkloadKind:       w.Kind,
		Zone:               pl.zone,
		Cloud:              pl.cloud,
		GpuCount:           clampU32(requested.GPUs),
		IntervalSec:        float32(s.interval(s.lastPod, string(p.UID), now, start).Seconds()),
		CpuUsageMillicores: clampU32(measured.CPUMillis),
		MemUsageBytes:      clampU64(measured.MemBytes),
	}
	if requested.GPUs > 0 {
		sample.GpuKind = pl.gpuKind
	}
	sample.Labels = s.labelsToSend(p, now)
	return sample, requested
}

// interval returns the wall time a sample for key covers and records
// now as key's last sample. started, when set, caps a first sample at
// the time since the pod started.
func (s *Scanner) interval(last map[string]time.Time, key string, now, started time.Time) time.Duration {
	d := s.cfg.Interval
	if prev, ok := last[key]; ok {
		d = now.Sub(prev)
	} else if !started.IsZero() && now.After(started) && now.Sub(started) < d {
		// A pod that started mid-interval only accrues since it started.
		d = now.Sub(started)
	}
	last[key] = now
	if d < 0 {
		d = 0
	}
	return min(d, maxInterval)
}

// labelsToSend returns the pod's labels on first sight, when they
// change, and every labelResend; nil otherwise.
func (s *Scanner) labelsToSend(p *corev1.Pod, now time.Time) map[string]string {
	if len(p.Labels) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.Labels))
	for k := range p.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxLabels {
		keys = keys[:maxLabels]
	}
	h := fnv.New64a()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(p.Labels[k]))
		_, _ = h.Write([]byte{0})
	}
	sum := h.Sum64()
	prev, ok := s.labelsSent[p.UID]
	if ok && prev.hash == sum && now.Sub(prev.sentAt) < labelResend {
		return nil
	}
	s.labelsSent[p.UID] = labelState{hash: sum, sentAt: now}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[k] = p.Labels[k]
	}
	return out
}

func clampU32(v int64) uint32 {
	if v <= 0 {
		return 0
	}
	if v > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(v)
}

func clampU64(v int64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64(v)
}

// ── /metrics chargeback series ────────────────────────────────────────

func podSeries(cluster string, s *kuberov1.PodCostSample) []metrics.Series {
	labels := map[string]string{
		"namespace":   s.Namespace,
		"pod":         s.Pod,
		"team":        s.Team,
		"cost_center": s.CostCenter,
		"nodepool":    s.Nodepool,
		"cloud":       s.Cloud,
		"region":      s.Region,
		"cluster":     cluster,
		"workload":    s.Workload,
	}
	if s.GpuKind != "" {
		labels["gpu_kind"] = s.GpuKind
	}
	return []metrics.Series{
		{Name: "kubehero_pod_cost_usd_per_second", Labels: labels, Value: s.CostUsdSec},
		{Name: "kubehero_pod_recoverable_usd_per_second", Labels: labels, Value: s.RecoverableUsdSec},
		{Name: "kubehero_pod_cpu_millicores", Labels: labels, Value: float64(s.CpuUsageMillicores)},
		{Name: "kubehero_pod_memory_bytes", Labels: labels, Value: float64(s.MemUsageBytes)},
	}
}

func nodeSeries(cluster string, n *kuberov1.NodeCostSample) metrics.Series {
	return metrics.Series{Name: "kubehero_node_cost_usd_per_hour", Labels: map[string]string{
		"node":         n.Node,
		"nodepool":     n.Nodepool,
		"cloud":        n.Cloud,
		"region":       n.Region,
		"sku":          n.Sku,
		"lifecycle":    n.Lifecycle,
		"price_source": n.PriceSource,
		"cluster":      cluster,
	}, Value: n.PricePerHour}
}
