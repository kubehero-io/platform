// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ingest

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
)

const gib = int64(1 << 30)

func approx(a, b float64) bool { return math.Abs(a-b) <= 1e-9 }

// ── fixtures ──────────────────────────────────────────────────────────

type fakeInventory struct {
	pods  []*corev1.Pod
	nodes map[string]*corev1.Node
}

func (f *fakeInventory) Pods() []*corev1.Pod           { return f.pods }
func (f *fakeInventory) Node(name string) *corev1.Node { return f.nodes[name] }
func (f *fakeInventory) Nodes() []*corev1.Node {
	out := make([]*corev1.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	return out
}

type fakeOwners struct{}

func (fakeOwners) Resolve(_ context.Context, p *corev1.Pod) kube.Workload {
	if w := p.Labels["app"]; w != "" {
		return kube.Workload{Name: w, Kind: "Deployment"}
	}
	return kube.Workload{Name: p.Name, Kind: "Pod"}
}

// fakeStats is a scriptable kubeletstats.Provider.
type fakeStats struct {
	mu    sync.Mutex
	res   map[string]*kubeletstats.Summary
	err   error
	calls map[string]int
}

func (f *fakeStats) Summary(_ context.Context, node string) (*kubeletstats.Summary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[node]++
	if f.err != nil {
		return nil, f.err
	}
	if s, ok := f.res[node]; ok {
		return s, nil
	}
	return &kubeletstats.Summary{Pods: map[kubeletstats.PodKey]kubeletstats.PodStats{}}, nil
}

func fakeNode(name, hourlyUSD string, cpuCores, memGiB int64) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Annotations: map[string]string{},
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.4xlarge",
				"topology.kubernetes.io/region":    "us-east-1",
				"topology.kubernetes.io/zone":      "us-east-1a",
				"kubehero.io/nodepool":             "platform",
			},
		},
		Spec: corev1.NodeSpec{ProviderID: "aws:///us-east-1a/i-0123"},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(memGiB*gib, resource.BinarySI),
		}},
	}
	if hourlyUSD != "" {
		n.Annotations[AnnotationNodeHourlyUSD] = hourlyUSD
	}
	return n
}

func fakePod(node, ns, name, cpu, mem string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{
				"app":                     "model-server",
				"kubehero.io/team":        "ml-platform",
				"kubehero.io/cost-center": "research",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(mem),
			}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func summaryFor(nodeCPU, nodeMem int64, pods map[*corev1.Pod]kubeletstats.Usage) *kubeletstats.Summary {
	s := &kubeletstats.Summary{
		Node: kubeletstats.NodeStats{CPUMillicores: nodeCPU, MemoryWorkingSetBytes: nodeMem, HasCPU: true, HasMemory: true},
		Pods: map[kubeletstats.PodKey]kubeletstats.PodStats{},
	}
	for p, u := range pods {
		s.Pods[kubeletstats.PodKey{Namespace: p.Namespace, Name: p.Name}] = kubeletstats.PodStats{UID: string(p.UID), Usage: u}
	}
	return s
}

func newScanner(inv Inventory, stats kubeletstats.Provider, node string) *Scanner {
	return New(Config{ClusterID: "eks-use1-prod", NodeName: node, Logger: slog.New(slog.DiscardHandler)},
		inv, fakeOwners{}, stats, NewPricer(nil, nil), nil)
}

// ── cost math ─────────────────────────────────────────────────────────

func TestScanRequestBasedCost(t *testing.T) {
	// $1.00/h node, 16 cores, 64 GiB. Pod requests 4 cores + 16 GiB →
	// 1/4 of both → $0.25/h, split $0.125 CPU + $0.125 RAM.
	n := fakeNode("n1", "1.00", 16, 64)
	p := fakePod("n1", "ml", "model-server-0", "4", "16Gi")
	inv := &fakeInventory{pods: []*corev1.Pod{p}, nodes: map[string]*corev1.Node{"n1": n}}
	stats := &fakeStats{err: errors.New("nodes/proxy forbidden")}
	req := newScanner(inv, stats, "n1").Scan(context.Background(), time.Now())
	if req == nil || len(req.Samples) != 1 || len(req.Nodes) != 1 {
		t.Fatalf("scan = %v", req)
	}
	s := req.Samples[0]
	if !approx(s.CostUsdSec, 0.25/3600) || !approx(s.CpuCostUsdSec, 0.125/3600) || !approx(s.RamCostUsdSec, 0.125/3600) || s.GpuCostUsdSec != 0 {
		t.Fatalf("cost = %v (cpu %v ram %v gpu %v)", s.CostUsdSec, s.CpuCostUsdSec, s.RamCostUsdSec, s.GpuCostUsdSec)
	}
	if !approx(s.CpuCostUsdSec+s.RamCostUsdSec+s.GpuCostUsdSec, s.CostUsdSec) {
		t.Fatal("breakdown must sum to the total")
	}
	// No measurement → no recoverable claim, usage fields zero.
	if s.RecoverableUsdSec != 0 || s.CpuUsageMillicores != 0 || s.MemUsageBytes != 0 {
		t.Fatalf("unmeasured pod: recoverable %v usage %d/%d", s.RecoverableUsdSec, s.CpuUsageMillicores, s.MemUsageBytes)
	}
	want := &kuberov1.PodCostSample{
		Cluster: "eks-use1-prod", Namespace: "ml", Pod: "model-server-0", Team: "ml-platform", CostCenter: "research",
		Nodepool: "platform", Node: "n1", Region: "us-east-1", Sku: "m5.4xlarge", Lifecycle: "on-demand",
		CpuMillicores: 4000, MemBytes: uint64(16 * gib), Workload: "model-server", WorkloadKind: "Deployment",
		Zone: "us-east-1a", Cloud: "aws", IntervalSec: 5,
	}
	if s.Cluster != want.Cluster || s.Team != want.Team || s.CostCenter != want.CostCenter || s.Nodepool != want.Nodepool ||
		s.Region != want.Region || s.Sku != want.Sku || s.Lifecycle != want.Lifecycle || s.CpuMillicores != want.CpuMillicores ||
		s.MemBytes != want.MemBytes || s.Workload != want.Workload || s.WorkloadKind != want.WorkloadKind ||
		s.Zone != want.Zone || s.Cloud != want.Cloud || s.IntervalSec != want.IntervalSec || s.Node != want.Node {
		t.Fatalf("sample = %+v\nwant   ~ %+v", s, want)
	}
	if req.ClusterId != "eks-use1-prod" || s.TsUnixMs == 0 {
		t.Fatalf("cluster id / ts not stamped: %q %d", req.ClusterId, s.TsUnixMs)
	}
}

func TestScanBlending(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	cases := []struct {
		name            string
		measured        *kubeletstats.Usage
		wantHourly      float64
		wantRecoverable float64 // $/h
	}{
		{name: "no kubelet stats falls back to requests", wantHourly: 0.25},
		{
			// Idle pod still bills its reservation; recoverable is the
			// priced unused part: (4000−120)/16000 × $0.5 + 15/64 × $0.5.
			name:            "idle pod bills its reservation",
			measured:        &kubeletstats.Usage{CPUMillicores: 120, MemoryWorkingSetBytes: 1 * gib},
			wantHourly:      0.25,
			wantRecoverable: 0.5*3880/16000 + 0.5*15/64,
		},
		{
			// CPU bursts to 8 cores over a 4-core request: 8/16 CPU,
			// 16/64 mem → (0.5+0.25)/2 = 0.375. Memory unused 14 GiB.
			name:            "cpu burst bills the overage",
			measured:        &kubeletstats.Usage{CPUMillicores: 8000, MemoryWorkingSetBytes: 2 * gib},
			wantHourly:      0.375,
			wantRecoverable: 0.5 * 14 / 64,
		},
		{
			name:            "memory overage bills the working set",
			measured:        &kubeletstats.Usage{CPUMillicores: 1000, MemoryWorkingSetBytes: 32 * gib},
			wantHourly:      0.375,
			wantRecoverable: 0.5 * 3000 / 16000,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := fakePod("n1", "ml", "model-server-0", "4", "16Gi")
			stats := &fakeStats{err: errors.New("down")}
			if c.measured != nil {
				stats = &fakeStats{res: map[string]*kubeletstats.Summary{"n1": summaryFor(0, 0, map[*corev1.Pod]kubeletstats.Usage{p: *c.measured})}}
			}
			inv := &fakeInventory{pods: []*corev1.Pod{p}, nodes: map[string]*corev1.Node{"n1": n}}
			s := newScanner(inv, stats, "n1").Scan(context.Background(), time.Now()).Samples[0]
			if !approx(s.CostUsdSec, c.wantHourly/3600) {
				t.Errorf("cost %v, want %v", s.CostUsdSec*3600, c.wantHourly)
			}
			if !approx(s.RecoverableUsdSec, c.wantRecoverable/3600) {
				t.Errorf("recoverable %v/h, want %v/h", s.RecoverableUsdSec*3600, c.wantRecoverable)
			}
			// cpu_millicores / mem_bytes stay the request snapshot; the
			// measurement goes to the usage fields.
			if s.CpuMillicores != 4000 || s.MemBytes != uint64(16*gib) {
				t.Errorf("request snapshot = %d / %d", s.CpuMillicores, s.MemBytes)
			}
			if c.measured != nil && (int64(s.CpuUsageMillicores) != c.measured.CPUMillicores || int64(s.MemUsageBytes) != c.measured.MemoryWorkingSetBytes) {
				t.Errorf("usage = %d / %d", s.CpuUsageMillicores, s.MemUsageBytes)
			}
		})
	}
}

// Stats keyed by name but from a previous incarnation of the pod (same
// StatefulSet name, new UID) must not be attributed.
func TestScanIgnoresStatsFromAnotherPodIncarnation(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	p := fakePod("n1", "db", "pg-0", "4", "16Gi")
	sum := summaryFor(0, 0, map[*corev1.Pod]kubeletstats.Usage{p: {CPUMillicores: 12000, MemoryWorkingSetBytes: 60 * gib}})
	ps := sum.Pods[kubeletstats.PodKey{Namespace: "db", Name: "pg-0"}]
	ps.UID = "old-incarnation"
	sum.Pods[kubeletstats.PodKey{Namespace: "db", Name: "pg-0"}] = ps
	inv := &fakeInventory{pods: []*corev1.Pod{p}, nodes: map[string]*corev1.Node{"n1": n}}
	s := newScanner(inv, &fakeStats{res: map[string]*kubeletstats.Summary{"n1": sum}}, "n1").Scan(context.Background(), time.Now()).Samples[0]
	if s.CpuUsageMillicores != 0 || !approx(s.CostUsdSec, 0.25/3600) {
		t.Fatalf("stale-incarnation stats were used: usage %d cost %v", s.CpuUsageMillicores, s.CostUsdSec*3600)
	}
}

// ── node locality (the N× fix) ────────────────────────────────────────

func TestScanIsNodeLocal(t *testing.T) {
	a, b := fakeNode("node-a", "1.00", 16, 64), fakeNode("node-b", "1.00", 16, 64)
	inv := &fakeInventory{
		// Even if the cache leaked another node's pod, it is not priced.
		pods: []*corev1.Pod{
			fakePod("node-a", "ns", "a1", "1", "1Gi"),
			fakePod("node-a", "ns", "a2", "1", "1Gi"),
			fakePod("node-b", "ns", "b1", "1", "1Gi"),
		},
		nodes: map[string]*corev1.Node{"node-a": a, "node-b": b},
	}
	stats := &fakeStats{}
	req := newScanner(inv, stats, "node-a").Scan(context.Background(), time.Now())
	if len(req.Samples) != 2 || len(req.Nodes) != 1 || req.Nodes[0].Node != "node-a" {
		t.Fatalf("node-local scan priced %d pods on %d nodes (%v)", len(req.Samples), len(req.Nodes), req.Nodes)
	}
	for _, s := range req.Samples {
		if s.Node != "node-a" {
			t.Fatalf("priced a pod from %s", s.Node)
		}
	}
	if stats.calls["node-b"] != 0 || stats.calls["node-a"] != 1 {
		t.Fatalf("kubelet summary calls = %v, want only node-a once", stats.calls)
	}

	// Dev mode (no NODE_NAME) prices every node, one summary each.
	stats = &fakeStats{}
	req = newScanner(inv, stats, "").Scan(context.Background(), time.Now())
	if len(req.Samples) != 3 || len(req.Nodes) != 2 || stats.calls["node-a"] != 1 || stats.calls["node-b"] != 1 {
		t.Fatalf("all-nodes scan: %d pods, %d nodes, calls %v", len(req.Samples), len(req.Nodes), stats.calls)
	}
}

func TestScanSkipsNonRunningAndUnknownNode(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	pending := fakePod("n1", "ns", "pending", "1", "1Gi")
	pending.Status.Phase = corev1.PodPending
	done := fakePod("n1", "ns", "done", "1", "1Gi")
	done.Status.Phase = corev1.PodSucceeded
	inv := &fakeInventory{pods: []*corev1.Pod{pending, done}, nodes: map[string]*corev1.Node{"n1": n}}
	req := newScanner(inv, &fakeStats{}, "n1").Scan(context.Background(), time.Now())
	if len(req.Samples) != 0 || len(req.Nodes) != 1 {
		t.Fatalf("non-running pods priced: %v", req.Samples)
	}
	// Node not cached yet: nothing to report, no panic.
	if got := newScanner(&fakeInventory{}, &fakeStats{}, "n1").Scan(context.Background(), time.Now()); got != nil {
		t.Fatalf("scan without node = %v", got)
	}
}

// ── node cost + idle ──────────────────────────────────────────────────

func TestNodeCostSampleAndIdle(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	p1 := fakePod("n1", "a", "p1", "4", "16Gi") // 1/4 of the node
	p2 := fakePod("n1", "b", "p2", "2", "8Gi")  // 1/8 of the node
	inv := &fakeInventory{pods: []*corev1.Pod{p1, p2}, nodes: map[string]*corev1.Node{"n1": n}}
	stats := &fakeStats{res: map[string]*kubeletstats.Summary{"n1": summaryFor(5200, 20*gib, nil)}}
	req := newScanner(inv, stats, "n1").Scan(context.Background(), time.Now())
	ns := req.Nodes[0]
	if ns.PricePerHour != 1 || ns.PriceSource != SourceAnnotation || !approx(ns.CostUsdSec, 1.0/3600) {
		t.Fatalf("price = %v (%s), cost/s %v", ns.PricePerHour, ns.PriceSource, ns.CostUsdSec)
	}
	if ns.CpuAllocatableMillicores != 16000 || ns.MemAllocatableBytes != uint64(64*gib) ||
		ns.CpuRequestedMillicores != 6000 || ns.MemRequestedBytes != uint64(24*gib) ||
		ns.CpuUsedMillicores != 5200 || ns.MemUsedBytes != uint64(20*gib) {
		t.Fatalf("node sample = %+v", ns)
	}
	// Pods were billed 3/8 of the node → idle is 5/8.
	if !approx(ns.IdleUsdSec, 0.625/3600) {
		t.Fatalf("idle = %v/h, want 0.625/h", ns.IdleUsdSec*3600)
	}
	if ns.Cloud != "aws" || ns.Zone != "us-east-1a" || ns.Sku != "m5.4xlarge" || ns.Lifecycle != "on-demand" || ns.Nodepool != "platform" || ns.IntervalSec != 5 {
		t.Fatalf("placement = %+v", ns)
	}
}

func TestIdleNeverNegative(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	hog := fakePod("n1", "a", "hog", "16", "64Gi")
	sum := summaryFor(0, 0, map[*corev1.Pod]kubeletstats.Usage{hog: {CPUMillicores: 20000, MemoryWorkingSetBytes: 64 * gib}})
	inv := &fakeInventory{pods: []*corev1.Pod{hog}, nodes: map[string]*corev1.Node{"n1": n}}
	req := newScanner(inv, &fakeStats{res: map[string]*kubeletstats.Summary{"n1": sum}}, "n1").Scan(context.Background(), time.Now())
	if req.Nodes[0].IdleUsdSec != 0 {
		t.Fatalf("idle = %v, must floor at 0", req.Nodes[0].IdleUsdSec)
	}
}

func TestGPUNodeAttribution(t *testing.T) {
	n := fakeNode("gpu-1", "10", 96, 1024)
	n.Labels["nvidia.com/gpu.product"] = "NVIDIA-A100-SXM4-80GB"
	n.Status.Allocatable["nvidia.com/gpu"] = resource.MustParse("8")
	trainer := fakePod("gpu-1", "ml", "trainer", "12", "128Gi")
	trainer.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	sidecar := fakePod("gpu-1", "ml", "exporter", "1", "4Gi")
	inv := &fakeInventory{pods: []*corev1.Pod{trainer, sidecar}, nodes: map[string]*corev1.Node{"gpu-1": n}}
	req := newScanner(inv, &fakeStats{err: errors.New("x")}, "gpu-1").Scan(context.Background(), time.Now())
	byPod := map[string]*kuberov1.PodCostSample{}
	for _, s := range req.Samples {
		byPod[s.Pod] = s
	}
	tr := byPod["trainer"]
	// 1/8 of every dimension of a $10/h node → $1.25/h, $0.875 of it GPU.
	if !approx(tr.CostUsdSec, 1.25/3600) || !approx(tr.GpuCostUsdSec, 0.875/3600) || tr.GpuCount != 1 || tr.GpuKind != "NVIDIA-A100-SXM4-80GB" {
		t.Fatalf("trainer = cost %v/h gpu %v/h count %d kind %q", tr.CostUsdSec*3600, tr.GpuCostUsdSec*3600, tr.GpuCount, tr.GpuKind)
	}
	ex := byPod["exporter"]
	if ex.GpuCostUsdSec != 0 || ex.GpuKind != "" || ex.GpuCount != 0 {
		t.Fatalf("non-GPU pod on GPU node = %+v", ex)
	}
	if req.Nodes[0].GpuCount != 8 || req.Nodes[0].GpuKind != "NVIDIA-A100-SXM4-80GB" {
		t.Fatalf("gpu node = %+v", req.Nodes[0])
	}
}

// ── interval + labels state ───────────────────────────────────────────

func TestIntervalTracking(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	old := fakePod("n1", "ns", "old", "1", "1Gi")
	young := fakePod("n1", "ns", "young", "1", "1Gi")
	t0 := time.Now()
	young.Status.StartTime = &metav1.Time{Time: t0.Add(-2 * time.Second)}
	inv := &fakeInventory{pods: []*corev1.Pod{old, young}, nodes: map[string]*corev1.Node{"n1": n}}
	sc := newScanner(inv, &fakeStats{}, "n1")

	get := func(req *kuberov1.IngestPodCostRequest, pod string) float32 {
		for _, s := range req.Samples {
			if s.Pod == pod {
				return s.IntervalSec
			}
		}
		t.Fatalf("pod %s missing", pod)
		return 0
	}
	r1 := sc.Scan(context.Background(), t0)
	if get(r1, "old") != 5 || math.Abs(float64(get(r1, "young"))-2) > 0.01 || r1.Nodes[0].IntervalSec != 5 {
		t.Fatalf("first intervals: old %v young %v node %v", get(r1, "old"), get(r1, "young"), r1.Nodes[0].IntervalSec)
	}
	// A slow scan: 7.5s later, both report the real gap.
	r2 := sc.Scan(context.Background(), t0.Add(7500*time.Millisecond))
	if get(r2, "old") != 7.5 || get(r2, "young") != 7.5 || r2.Nodes[0].IntervalSec != 7.5 {
		t.Fatalf("second intervals: %v %v %v", get(r2, "old"), get(r2, "young"), r2.Nodes[0].IntervalSec)
	}
	// A frozen process claims at most maxInterval.
	r3 := sc.Scan(context.Background(), t0.Add(time.Hour))
	if get(r3, "old") != float32(maxInterval.Seconds()) {
		t.Fatalf("capped interval = %v", get(r3, "old"))
	}
	// Pods that disappear are forgotten (bounded state).
	inv.pods = []*corev1.Pod{old}
	sc.Scan(context.Background(), t0.Add(time.Hour+5*time.Second))
	if _, ok := sc.lastPod[string(young.UID)]; ok {
		t.Fatal("state for a vanished pod must be dropped")
	}
}

func TestLabelsSentOnFirstSightAndChange(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	p := fakePod("n1", "ns", "p", "1", "1Gi")
	inv := &fakeInventory{pods: []*corev1.Pod{p}, nodes: map[string]*corev1.Node{"n1": n}}
	sc := newScanner(inv, &fakeStats{}, "n1")
	t0 := time.Now()
	if l := sc.Scan(context.Background(), t0).Samples[0].Labels; l["app"] != "model-server" || len(l) != 3 {
		t.Fatalf("first sight labels = %v", l)
	}
	if l := sc.Scan(context.Background(), t0.Add(5*time.Second)).Samples[0].Labels; l != nil {
		t.Fatalf("unchanged labels resent: %v", l)
	}
	p2 := p.DeepCopy()
	p2.Labels["version"] = "v2"
	inv.pods = []*corev1.Pod{p2}
	if l := sc.Scan(context.Background(), t0.Add(10*time.Second)).Samples[0].Labels; l["version"] != "v2" {
		t.Fatalf("changed labels not sent: %v", l)
	}
	if l := sc.Scan(context.Background(), t0.Add(15*time.Second)).Samples[0].Labels; l != nil {
		t.Fatalf("labels resent without change: %v", l)
	}
	if l := sc.Scan(context.Background(), t0.Add(labelResend+time.Minute)).Samples[0].Labels; l == nil {
		t.Fatal("labels must be refreshed after labelResend")
	}
}

func TestKubeletFailureWarnsOnceAndRecovers(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	p := fakePod("n1", "ns", "p", "1", "1Gi")
	inv := &fakeInventory{pods: []*corev1.Pod{p}, nodes: map[string]*corev1.Node{"n1": n}}
	stats := &fakeStats{err: errors.New("forbidden")}
	h := &countingHandler{}
	sc := New(Config{NodeName: "n1", Logger: slog.New(h)}, inv, fakeOwners{}, stats, nil, nil)
	for range 3 {
		sc.Scan(context.Background(), time.Now())
	}
	if h.warns.Load() != 1 {
		t.Fatalf("warns = %d, want 1", h.warns.Load())
	}
	stats.mu.Lock()
	stats.err = nil
	stats.res = map[string]*kubeletstats.Summary{"n1": summaryFor(1, 1, map[*corev1.Pod]kubeletstats.Usage{p: {CPUMillicores: 3000}})}
	stats.mu.Unlock()
	if s := sc.Scan(context.Background(), time.Now()).Samples[0]; s.CpuUsageMillicores != 3000 {
		t.Fatalf("recovered usage = %d", s.CpuUsageMillicores)
	}
	stats.mu.Lock()
	stats.err = errors.New("again")
	stats.mu.Unlock()
	sc.Scan(context.Background(), time.Now())
	if h.warns.Load() != 2 {
		t.Fatalf("relapse must warn again, warns = %d", h.warns.Load())
	}
}

type countingHandler struct{ warns atomic.Int32 }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns.Add(1)
	}
	return nil
}
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func TestSeriesExposeChargebackLabels(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	inv := &fakeInventory{pods: []*corev1.Pod{fakePod("n1", "ml", "p", "4", "16Gi")}, nodes: map[string]*corev1.Node{"n1": n}}
	sc := newScanner(inv, &fakeStats{}, "n1")
	sc.Scan(context.Background(), time.Now())
	names := map[string]map[string]string{}
	for _, s := range sc.Series() {
		names[s.Name] = s.Labels
	}
	for _, need := range []string{"team", "namespace", "pod", "nodepool", "cloud", "region", "cost_center", "cluster"} {
		if _, ok := names["kubehero_pod_cost_usd_per_second"][need]; !ok {
			t.Errorf("kubehero_pod_cost_usd_per_second missing label %q", need)
		}
	}
	if names["kubehero_node_cost_usd_per_hour"]["price_source"] != SourceAnnotation {
		t.Errorf("node series = %v", names["kubehero_node_cost_usd_per_hour"])
	}
}

func TestRunEmitsEveryTick(t *testing.T) {
	n := fakeNode("n1", "1.00", 16, 64)
	inv := &fakeInventory{pods: []*corev1.Pod{fakePod("n1", "ml", "p", "1", "1Gi")}, nodes: map[string]*corev1.Node{"n1": n}}
	got := make(chan *kuberov1.IngestPodCostRequest, 10)
	sc := New(Config{NodeName: "n1", Interval: 10 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)},
		inv, fakeOwners{}, &fakeStats{}, nil, func(r *kuberov1.IngestPodCostRequest) { got <- r })
	ctx, cancel := context.WithCancel(context.Background())
	go sc.Run(ctx)
	for range 3 {
		select {
		case <-got:
		case <-time.After(2 * time.Second):
			t.Fatal("scanner did not emit")
		}
	}
	cancel()
}

// ── pricing ───────────────────────────────────────────────────────────

type fakePricing struct {
	kuberov1connect.UnimplementedPricingServiceHandler
	calls    atomic.Int32
	fail     atomic.Bool
	price    float64
	currency string
	last     atomic.Pointer[kuberov1.QuoteRequest]
}

func (f *fakePricing) Quote(_ context.Context, req *connect.Request[kuberov1.QuoteRequest]) (*connect.Response[kuberov1.QuoteResponse], error) {
	f.calls.Add(1)
	f.last.Store(req.Msg)
	if f.fail.Load() {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("catalog down"))
	}
	return connect.NewResponse(&kuberov1.QuoteResponse{PricePerHour: f.price, Currency: f.currency}), nil
}

func pricingServer(t *testing.T, f *fakePricing) kuberov1connect.PricingServiceClient {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(kuberov1connect.NewPricingServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return kuberov1connect.NewPricingServiceClient(srv.Client(), srv.URL)
}

func TestPricerOrderAndCache(t *testing.T) {
	f := &fakePricing{price: 0.768, currency: "USD"}
	pr := NewPricer(pricingServer(t, f), slog.New(slog.DiscardHandler))
	now := time.Now()
	pr.now = func() time.Time { return now }

	n := fakeNode("n1", "", 16, 64)
	n.Labels["eks.amazonaws.com/capacityType"] = "SPOT"
	price, src := pr.Price(context.Background(), n)
	if price != 0.768 || src != SourcePricingEngine {
		t.Fatalf("Price = %v %s", price, src)
	}
	q := f.last.Load()
	if q.GetCloud() != "aws" || q.GetSku() != "m5.4xlarge" || q.GetRegion() != "us-east-1" || q.GetLifecycle() != "spot" {
		t.Fatalf("quote request = %+v", q)
	}
	pr.Price(context.Background(), n)
	if f.calls.Load() != 1 {
		t.Fatalf("quotes within TTL = %d, want 1", f.calls.Load())
	}
	// The annotation wins over the engine.
	n2 := n.DeepCopy()
	n2.Annotations[AnnotationNodeHourlyUSD] = "0.5"
	if p, s := pr.Price(context.Background(), n2); p != 0.5 || s != SourceAnnotation {
		t.Fatalf("annotation price = %v %s", p, s)
	}
	// After the TTL a failing engine keeps the last good quote…
	f.fail.Store(true)
	now = now.Add(2 * time.Hour)
	if p, s := pr.Price(context.Background(), n); p != 0.768 || s != SourcePricingEngine {
		t.Fatalf("stale quote = %v %s", p, s)
	}
	// …and is not asked again inside the negative-cache window.
	calls := f.calls.Load()
	pr.Price(context.Background(), n)
	if f.calls.Load() != calls {
		t.Fatal("failed quote must be negatively cached")
	}
}

func TestPricerFallsBackToEstimate(t *testing.T) {
	f := &fakePricing{fail: atomic.Bool{}}
	f.fail.Store(true)
	pr := NewPricer(pricingServer(t, f), slog.New(slog.DiscardHandler))
	n := fakeNode("n1", "", 4, 16)
	if p, s := pr.Price(context.Background(), n); s != SourceEstimate || !approx(p, 4*0.04+16*0.005) {
		t.Fatalf("estimate = %v %s", p, s)
	}
	// Missing SKU: the engine isn't even asked.
	f2 := &fakePricing{price: 1}
	pr2 := NewPricer(pricingServer(t, f2), nil)
	n3 := fakeNode("n3", "", 4, 16)
	delete(n3.Labels, "node.kubernetes.io/instance-type")
	if _, s := pr2.Price(context.Background(), n3); s != SourceEstimate || f2.calls.Load() != 0 {
		t.Fatalf("no-sku node: source %s, calls %d", s, f2.calls.Load())
	}
	// A non-USD or non-positive quote is not trusted.
	f3 := &fakePricing{price: 0.9, currency: "EUR"}
	if _, s := NewPricer(pricingServer(t, f3), slog.New(slog.DiscardHandler)).Price(context.Background(), fakeNode("n", "", 4, 16)); s != SourceEstimate {
		t.Fatalf("EUR quote accepted: %s", s)
	}
}

func TestEstimateAndAnnotationParsing(t *testing.T) {
	n := fakeNode("g", "", 8, 32)
	n.Status.Allocatable["nvidia.com/gpu"] = resource.MustParse("2")
	if got := Estimate(n); !approx(got, 8*0.04+32*0.005+2*1.5) {
		t.Fatalf("gpu estimate = %v", got)
	}
	for in, ok := range map[string]bool{"0.192": true, " 2 ": true, "0": false, "-1": false, "abc": false, "1e9": false, "NaN": false, "0.19usd": false} {
		if _, got := parsePrice(in); got != ok {
			t.Errorf("parsePrice(%q) ok = %v", in, got)
		}
	}
	// Nil node: honest zero, never a panic.
	if p, _ := NewPricer(nil, nil).Price(context.Background(), nil); p != 0 {
		t.Fatalf("nil node price = %v", p)
	}
}
