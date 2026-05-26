// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package ingest is the collector's data plane. It scans the Kubernetes
// API for running Pods every `interval`, attributes a synthetic cost
// per Pod based on resource requests + node SKU, and ships the batch
// to the control-plane via Connect-RPC.
//
// Architectural notes:
//
//   - Synthetic cost, not eBPF. The Pod's `requests.cpu/memory` get
//     multiplied by a per-node hourly rate; we divide by the scan
//     window to land on $/sec. This is honest enough for cluster-
//     aware accounting, dishonest for utilisation. eBPF lands later
//     and replaces this same struct shape with measured numbers.
//
//   - We call `Pods("").List()` per scan rather than a long-running
//     informer. Cost is proportional to number of pods, not change
//     rate; for a 5s tick on a 200-pod cluster this is one HTTP round
//     trip every 5s, well under the kube-apiserver's load budget.
//
//   - Node SKU is read from a node annotation set by the cloud's
//     node-bootstrap script. We fall back to the node's instance-type
//     label (well-known per cloud) when the annotation is missing.

package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Config carries every knob the scanner needs. Populated from env in
// main.go.
type Config struct {
	// ControlPlaneURL is the cp's HTTP base. Empty disables emit.
	ControlPlaneURL string
	// Token is the bearer the cp accepts (member+ role).
	Token string
	// ClusterID — typically a slug like "eks-use1-prod" — gets stamped
	// on every sample so the cp knows which cluster owns the row.
	ClusterID string
	// Interval between scans. Defaults to 5s; faster than that
	// thrashes the apiserver, slower drops accuracy under bursty
	// scaling.
	Interval time.Duration
	// Logger is taken from the parent process.
	Logger *slog.Logger
}

// Run blocks until ctx is cancelled. Each tick: list pods, attribute
// cost, ship a batch.
func Run(ctx context.Context, cfg Config) error {
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.ClusterID == "" {
		log.Warn("CLUSTER_ID unset — samples will be dropped server-side")
	}

	k8s, err := kubeClient()
	if err != nil {
		return fmt.Errorf("kube client: %w", err)
	}

	httpc := &http.Client{Timeout: 8 * time.Second}

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

	// Run immediately + at every tick.
	scan(ctx, log, k8s, httpc, cfg)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			scan(ctx, log, k8s, httpc, cfg)
		}
	}
}

func scan(ctx context.Context, log *slog.Logger, k8s *kubernetes.Clientset, httpc *http.Client, cfg Config) {
	// One round-trip listing every pod across every namespace. The
	// apiserver caches this; we're light on it.
	pods, err := k8s.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Warn("pod list failed", "err", err)
		return
	}
	if len(pods.Items) == 0 {
		return
	}

	// Pre-fetch nodes once per scan and index by name — every Pod
	// needs its node's labels for SKU + region + nodepool attribution.
	nodeIndex := map[string]*corev1.Node{}
	if nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err == nil {
		for i := range nodes.Items {
			nodeIndex[nodes.Items[i].Name] = &nodes.Items[i]
		}
	}

	now := time.Now().UnixMilli()
	samples := make([]map[string]any, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		s := attributePod(p, nodeIndex[p.Spec.NodeName], cfg.Interval)
		s["tsUnixMs"] = now
		s["cluster"] = cfg.ClusterID
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return
	}

	if cfg.ControlPlaneURL == "" {
		// Local dev / kind-demo without a control plane — log the
		// count so operators can see ingest is firing without a sink.
		log.Info("scan complete (no cp wired)", "samples", len(samples))
		return
	}

	if err := emit(ctx, httpc, cfg, samples); err != nil {
		log.Warn("emit failed", "err", err, "samples", len(samples))
		return
	}
	log.Info("ingested", "samples", len(samples))
}

// attributePod computes a per-second cost from request × node-rate.
// The math is deliberately simple — production replaces it with eBPF
// utilisation × node-rate, but the wire shape is unchanged.
func attributePod(p *corev1.Pod, n *corev1.Node, interval time.Duration) map[string]any {
	cpuMilli, memBytes := podRequests(p)
	cpuCores := float64(cpuMilli) / 1000

	hourlyUSD := nodeHourlyUSD(n) // $/hr for the whole node
	cpuPerCorePerHour := hourlyUSD / float64(max32(nodeAllocatableCPU(n), 1))
	memPerGiBPerHour := (hourlyUSD * 0.4) / float64(max32(nodeAllocatableMemGiB(n), 1)) // memory ≈ 40% of node cost

	hourly := cpuCores*cpuPerCorePerHour + (float64(memBytes)/1024/1024/1024)*memPerGiBPerHour
	costSec := hourly / 3600

	team := p.Labels["kubehero.io/team"]
	if team == "" {
		team = p.Namespace
	}
	cc := p.Labels["kubehero.io/cost-center"]

	out := map[string]any{
		"namespace":      p.Namespace,
		"pod":            p.Name,
		"team":           team,
		"costCenter":     cc,
		"node":           p.Spec.NodeName,
		"cpuMillicores":  cpuMilli,
		"memBytes":       memBytes,
		"costUsdSec":     costSec,
	}
	if n != nil {
		out["nodepool"] = nodepoolOf(n)
		out["region"] = n.Labels["topology.kubernetes.io/region"]
		out["sku"] = nodeSKU(n)
		out["lifecycle"] = nodeLifecycle(n)
		_ = interval // kept for future window-aware attribution
	}
	return out
}

func podRequests(p *corev1.Pod) (uint32, uint64) {
	var cpu, mem int64
	for _, c := range p.Spec.Containers {
		if v, ok := c.Resources.Requests[corev1.ResourceCPU]; ok {
			cpu += v.MilliValue()
		}
		if v, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			mem += v.Value()
		}
	}
	if cpu < 0 {
		cpu = 0
	}
	if mem < 0 {
		mem = 0
	}
	return uint32(cpu), uint64(mem)
}

// nodeHourlyUSD reads the per-node hourly price from a kubehero
// annotation set during cluster registration, or falls back to a
// rough estimate scaled to the node's allocatable. Real production
// replaces this with a price lookup against the pricing-engine.
func nodeHourlyUSD(n *corev1.Node) float64 {
	if n == nil {
		return 0.10 // safe-but-cheap fallback
	}
	if a, ok := n.Annotations["kubehero.io/node-hourly-usd"]; ok {
		if v, err := parseFloat(a); err == nil && v > 0 {
			return v
		}
	}
	cores := nodeAllocatableCPU(n)
	gib := nodeAllocatableMemGiB(n)
	if cores < 1 {
		cores = 1
	}
	if gib < 1 {
		gib = 1
	}
	// Rough $0.04/core/hr + $0.005/GiB/hr — close to AWS m5 family.
	return float64(cores)*0.04 + float64(gib)*0.005
}

func nodeAllocatableCPU(n *corev1.Node) float32 {
	if n == nil {
		return 0
	}
	q, ok := n.Status.Allocatable[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	return float32(q.MilliValue()) / 1000
}

func nodeAllocatableMemGiB(n *corev1.Node) float32 {
	if n == nil {
		return 0
	}
	q, ok := n.Status.Allocatable[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return float32(q.Value()) / float32(1024*1024*1024)
}

// nodepoolOf reads the cloud-vendor-specific nodepool label, falling
// back to our canonical kubehero.io/nodepool label.
func nodepoolOf(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	for _, k := range []string{
		"kubehero.io/nodepool",
		"eks.amazonaws.com/nodegroup",
		"cloud.google.com/gke-nodepool",
		"agentpool",
		"karpenter.sh/nodepool",
	} {
		if v := n.Labels[k]; v != "" {
			return v
		}
	}
	return ""
}

func nodeSKU(n *corev1.Node) string {
	if n == nil {
		return ""
	}
	if v := n.Labels["node.kubernetes.io/instance-type"]; v != "" {
		return v
	}
	return n.Labels["beta.kubernetes.io/instance-type"]
}

func nodeLifecycle(n *corev1.Node) string {
	if n == nil {
		return "on-demand"
	}
	if v := n.Labels["karpenter.sh/capacity-type"]; v != "" {
		return v
	}
	if v := n.Labels["eks.amazonaws.com/capacityType"]; v != "" {
		return strings.ToLower(v)
	}
	if v := n.Labels["cloud.google.com/gke-spot"]; v == "true" {
		return "spot"
	}
	return "on-demand"
}

func parseFloat(s string) (float64, error) {
	var v float64
	_, err := fmt.Sscanf(s, "%f", &v)
	return v, err
}

func max32(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}

// emit POSTs the batch to /kubehero.v1.ControlPlaneService/IngestPodCost.
// Plain HTTP+JSON wire — Connect-RPC accepts that natively, and we
// don't pull the generated Connect-Go stubs into the collector to
// keep the binary lean.
func emit(ctx context.Context, httpc *http.Client, cfg Config, samples []map[string]any) error {
	body, _ := json.Marshal(map[string]any{
		"clusterId": cfg.ClusterID,
		"samples":   samples,
	})
	url := strings.TrimRight(cfg.ControlPlaneURL, "/") +
		"/kubehero.v1.ControlPlaneService/IngestPodCost"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// kubeClient picks in-cluster config first (the DaemonSet path), then
// falls back to KUBECONFIG (local dev). This is the canonical
// client-go pattern.
func kubeClient() (*kubernetes.Clientset, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to KUBECONFIG / ~/.kube/config for local dev.
		cfg, err = clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
		if err != nil {
			return nil, err
		}
	}
	cfg.UserAgent = "kubehero-collector/0.1"
	return kubernetes.NewForConfig(cfg)
}

// Used only to keep resource.Quantity reachable for future tighter
// attribution (e.g. burstable QoS).
var _ resource.Quantity
