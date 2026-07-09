// SPDX-License-Identifier: Apache-2.0
package ingest

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	costmodel "github.com/kubehero-io/platform/packages/cost-model"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
)

// fakeNode builds a Node with allocatable cpu/memory + a node hourly
// price annotation so the cost math is deterministic.
func fakeNode(t *testing.T, hourlyUSD string, cpuCores int64, memGiB int64) *corev1.Node {
	t.Helper()
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "n1",
			Annotations: map[string]string{
				"kubehero.io/node-hourly-usd": hourlyUSD,
			},
			Labels: map[string]string{
				"node.kubernetes.io/instance-type": "m5.4xlarge",
				"topology.kubernetes.io/region":    "us-east-1",
				"kubehero.io/nodepool":             "platform",
			},
		},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(cpuCores, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(memGiB*1024*1024*1024, resource.BinarySI),
			},
		},
	}
	return n
}

func fakePod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ml",
			Name:      "model-server-a100-0",
			Labels: map[string]string{
				"kubehero.io/team":        "ml-platform",
				"kubehero.io/cost-center": "research",
			},
		},
		Spec: corev1.PodSpec{
			NodeName: "n1",
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func TestAttributePodRequestBasedCost(t *testing.T) {
	// $1.00 / hr node, 16 cores, 64 GiB allocatable. Pod requests
	// 4 cores + 16 GiB → cpuShare 0.25, memShare 0.25 → blended 0.25
	// of the node → $0.25/hr → $0.25/3600 per second (cost-model math).
	n := fakeNode(t, "1.00", 16, 64)
	out := attributePod(fakePod(), n, costmodel.PodShare{}, false)

	cost, _ := out["costUsdSec"].(float64)
	if cost <= 0 {
		t.Fatalf("expected positive cost, got %v", cost)
	}
	wantSec := 0.25 / 3600
	if delta := cost - wantSec; delta > 1e-9 || delta < -1e-9 {
		t.Errorf("cost %v vs expected %v (delta %v)", cost, wantSec, delta)
	}
	if out["team"] != "ml-platform" {
		t.Errorf("team label not propagated: %v", out["team"])
	}
	if out["nodepool"] != "platform" {
		t.Errorf("nodepool label not propagated: %v", out["nodepool"])
	}
	if out["sku"] != "m5.4xlarge" {
		t.Errorf("sku label not propagated: %v", out["sku"])
	}
}

func TestAttributePodBlending(t *testing.T) {
	gib := int64(1024 * 1024 * 1024)
	n := fakeNode(t, "1.00", 16, 64) // $1/hr, 16 cores, 64 GiB
	cases := []struct {
		name       string
		measured   costmodel.PodShare
		measuredOK bool
		wantHourly float64
	}{
		{
			// Requests only: 4/16 cpu + 16/64 mem → 0.25 blended.
			name:       "no kubelet stats falls back to requests",
			measuredOK: false,
			wantHourly: 0.25,
		},
		{
			// Usage below requests: bill stays floored at requests.
			name:       "idle pod still bills its reservation",
			measured:   costmodel.PodShare{CPUMillis: 120, MemBytes: 1 * gib},
			measuredOK: true,
			wantHourly: 0.25,
		},
		{
			// CPU bursts to 8 cores over a 4-core request: cpuShare
			// 8/16=0.5, memShare 16/64=0.25 → blended 0.375.
			name:       "cpu burst above requests bills the overage",
			measured:   costmodel.PodShare{CPUMillis: 8000, MemBytes: 2 * gib},
			measuredOK: true,
			wantHourly: 0.375,
		},
		{
			// Memory working set 32 GiB over a 16 GiB request:
			// cpuShare 0.25, memShare 32/64=0.5 → blended 0.375.
			name:       "memory overage bills the working set",
			measured:   costmodel.PodShare{CPUMillis: 1000, MemBytes: 32 * gib},
			measuredOK: true,
			wantHourly: 0.375,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := attributePod(fakePod(), n, c.measured, c.measuredOK)
			cost, _ := out["costUsdSec"].(float64)
			wantSec := c.wantHourly / 3600
			if delta := cost - wantSec; delta > 1e-9 || delta < -1e-9 {
				t.Errorf("cost %v vs expected %v (delta %v)", cost, wantSec, delta)
			}
			// Wire format is unchanged until the proto gains usage
			// fields: cpuMillicores/memBytes stay the request snapshot.
			if got := out["cpuMillicores"].(uint32); got != 4000 {
				t.Errorf("cpuMillicores = %d, want request snapshot 4000", got)
			}
			if got := out["memBytes"].(uint64); got != uint64(16*gib) {
				t.Errorf("memBytes = %d, want request snapshot %d", got, 16*gib)
			}
		})
	}
}

func TestAttributePodFallsBackToNamespaceTeam(t *testing.T) {
	// No team label — defaults to namespace.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "edge", Name: "api-1"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("100m"),
					},
				},
			}},
		},
	}
	out := attributePod(pod, nil, costmodel.PodShare{}, false)
	if out["team"] != "edge" {
		t.Fatalf("team should default to namespace, got %v", out["team"])
	}
	// No node → no priced hardware → $0, never a panic.
	if cost, _ := out["costUsdSec"].(float64); cost != 0 {
		t.Fatalf("nodeless pod should cost 0, got %v", cost)
	}
}

func TestNodepoolReadsCloudVendorLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{"kubehero own", map[string]string{"kubehero.io/nodepool": "ml-a100"}, "ml-a100"},
		{"eks", map[string]string{"eks.amazonaws.com/nodegroup": "spot-batch"}, "spot-batch"},
		{"gke", map[string]string{"cloud.google.com/gke-nodepool": "default"}, "default"},
		{"aks", map[string]string{"agentpool": "user1"}, "user1"},
		{"karpenter", map[string]string{"karpenter.sh/nodepool": "spot"}, "spot"},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: c.labels}}
			if got := nodepoolOf(n); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestNodeHourlyUSDFallsBackWhenAnnotationMissing(t *testing.T) {
	// 4 cores, 16 GiB allocatable, no annotation.
	n := fakeNode(t, "", 4, 16)
	delete(n.Annotations, "kubehero.io/node-hourly-usd")
	got := nodeHourlyUSD(n)
	// 4 × 0.04 + 16 × 0.005 = 0.24
	want := 0.24
	if delta := got - want; delta > 1e-9 || delta < -1e-9 {
		t.Fatalf("fallback nodeHourlyUSD = %v, want %v", got, want)
	}
}

// ── kubelet stats degradation ──────────────────────────────────────────

// fakeUsage is a scriptable kubeletstats.Provider.
type fakeUsage struct {
	res   map[string]map[kubeletstats.PodKey]kubeletstats.Usage
	err   error
	calls int
}

func (f *fakeUsage) PodUsage(_ context.Context, node string) (map[kubeletstats.PodKey]kubeletstats.Usage, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.res[node], nil
}

// recordingHandler counts slog records per level.
type recordingHandler struct {
	mu   sync.Mutex
	msgs map[slog.Level][]string
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{msgs: map[slog.Level][]string{}}
}
func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs[r.Level] = append(h.msgs[r.Level], r.Message)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) count(l slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.msgs[l])
}

func TestUsageForNodesWarnsOncePerNode(t *testing.T) {
	fu := &fakeUsage{err: errors.New("nodes \"n1\" is forbidden: cannot get resource \"nodes/proxy\"")}
	st := newScanState(fu)
	h := newRecordingHandler()
	log := slog.New(h)

	// Three consecutive failing scans → exactly one warn.
	for range 3 {
		got := st.usageForNodes(context.Background(), log, []string{"n1"})
		if len(got) != 0 {
			t.Fatalf("failing node must contribute no usage, got %v", got)
		}
	}
	if h.count(slog.LevelWarn) != 1 {
		t.Fatalf("expected exactly 1 warn for a persistently failing node, got %d", h.count(slog.LevelWarn))
	}

	// Recovery clears the latch (and says so at info)…
	fu.err = nil
	fu.res = map[string]map[kubeletstats.PodKey]kubeletstats.Usage{
		"n1": {{Namespace: "ml", Name: "p1"}: {CPUMillicores: 100, MemoryWorkingSetBytes: 1024}},
	}
	got := st.usageForNodes(context.Background(), log, []string{"n1"})
	if len(got["n1"]) != 1 {
		t.Fatalf("expected recovered usage for n1, got %v", got)
	}

	// …so a relapse warns again.
	fu.err = errors.New("kubelet stopped serving stats")
	st.usageForNodes(context.Background(), log, []string{"n1"})
	if h.count(slog.LevelWarn) != 2 {
		t.Fatalf("expected a second warn after recovery+relapse, got %d", h.count(slog.LevelWarn))
	}
}

func TestUsageForNodesIndependentNodes(t *testing.T) {
	fu := &fakeUsage{
		res: map[string]map[kubeletstats.PodKey]kubeletstats.Usage{
			"n1": {{Namespace: "edge", Name: "api-1"}: {CPUMillicores: 41, MemoryWorkingSetBytes: 73400320}},
			// n2 absent — kubelet returned an empty summary.
		},
	}
	st := newScanState(fu)
	got := st.usageForNodes(context.Background(), slog.New(newRecordingHandler()), []string{"n1", "n2"})
	if fu.calls != 2 {
		t.Fatalf("expected one summary call per node, got %d", fu.calls)
	}
	if u := got["n1"][kubeletstats.PodKey{Namespace: "edge", Name: "api-1"}]; u.CPUMillicores != 41 {
		t.Fatalf("n1 usage not propagated: %+v", got)
	}
}
