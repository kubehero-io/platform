// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package usage

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
)

type inv struct {
	pods  []*corev1.Pod
	nodes map[string]*corev1.Node
}

func (i *inv) Pods() []*corev1.Pod           { return i.pods }
func (i *inv) Node(n string) *corev1.Node    { return i.nodes[n] }
func (i *inv) Nodes() []*corev1.Node         { return nil }
func (i *inv) setPods(p ...*corev1.Pod) *inv { i.pods = p; return i }

type owners struct{}

func (owners) Resolve(context.Context, *corev1.Pod) kube.Workload {
	return kube.Workload{Name: "checkout", Kind: "Deployment"}
}

type stats struct {
	sum   map[string]*kubeletstats.Summary
	err   error
	calls []string
}

func (s *stats) Summary(_ context.Context, node string) (*kubeletstats.Summary, error) {
	s.calls = append(s.calls, node)
	if s.err != nil {
		return nil, s.err
	}
	return s.sum[node], nil
}

func res(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(mem)}
}

func testPod(node string) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "checkout-abc", UID: "u1", Labels: map[string]string{"kubehero.io/team": "payments"}},
		Spec: corev1.PodSpec{
			NodeName: node,
			InitContainers: []corev1.Container{
				{Name: "migrate", Resources: corev1.ResourceRequirements{Requests: res("1", "1Gi")}},
				{Name: "proxy", RestartPolicy: &always, Resources: corev1.ResourceRequirements{Requests: res("100m", "64Mi")}},
			},
			Containers: []corev1.Container{
				{Name: "app", Resources: corev1.ResourceRequirements{Requests: res("500m", "256Mi"), Limits: res("2", "512Mi")}},
				{Name: "warming", Resources: corev1.ResourceRequirements{Requests: res("100m", "32Mi")}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "app", RestartCount: 3,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}},
			}},
		},
	}
}

func testSummary() *kubeletstats.Summary {
	return &kubeletstats.Summary{Pods: map[kubeletstats.PodKey]kubeletstats.PodStats{
		{Namespace: "shop", Name: "checkout-abc"}: {UID: "u1", Containers: map[string]kubeletstats.ContainerUsage{
			"app":     {CPUNanoCores: 412_500_000, MemoryWorkingSetBytes: 300 << 20, HasCPU: true, HasMemory: true},
			"proxy":   {CPUNanoCores: 5_000_000, MemoryWorkingSetBytes: 40 << 20, HasCPU: true, HasMemory: true},
			"warming": {MemoryWorkingSetBytes: 1 << 20, HasMemory: true}, // cpu not sampled yet
		}},
	}}
}

func TestSampleJoinsSpecStatusAndStats(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "z1"}}}
	st := &stats{sum: map[string]*kubeletstats.Summary{"node-a": testSummary()}}
	s := New(Config{ClusterID: "c1", NodeName: "node-a", Logger: slog.New(slog.DiscardHandler)},
		(&inv{nodes: map[string]*corev1.Node{"node-a": node}}).setPods(testPod("node-a")), owners{}, st, nil)
	now := time.UnixMilli(1_760_000_000_000)
	req := s.Sample(context.Background(), now)
	if req == nil || req.ClusterId != "c1" {
		t.Fatalf("request = %v", req)
	}
	byName := map[string]*kuberov1.ContainerUsage{}
	for _, u := range req.Usage {
		byName[u.GetSource().GetContainer()] = u
	}
	// app + native sidecar; the exited init container and the
	// not-yet-sampled container are skipped.
	if len(byName) != 2 || byName["app"] == nil || byName["proxy"] == nil {
		t.Fatalf("rows for %v", byName)
	}
	app := byName["app"]
	if app.CpuUsageCores != 0.4125 || app.MemWorkingSetBytes != 300<<20 ||
		app.CpuRequestCores != 0.5 || app.MemRequestBytes != 256<<20 ||
		app.CpuLimitCores != 2 || app.MemLimitBytes != 512<<20 ||
		app.Restarts != 3 || app.LastTerminationReason != "OOMKilled" || app.TsUnixMs != now.UnixMilli() {
		t.Fatalf("app row = %+v", app)
	}
	src := app.GetSource()
	if src.GetNamespace() != "shop" || src.GetPod() != "checkout-abc" || src.GetWorkload() != "checkout" ||
		src.GetWorkloadKind() != "Deployment" || src.GetTeam() != "payments" || src.GetZone() != "z1" || src.GetNode() != "node-a" || src.GetPodUid() != "u1" {
		t.Fatalf("source = %+v", src)
	}
	if p := byName["proxy"]; p.CpuLimitCores != 0 || p.MemLimitBytes != 0 {
		t.Fatalf("no limit must read as 0: %+v", p)
	}
}

func TestSampleSkipsOtherNodesAndStaleIncarnations(t *testing.T) {
	sum := testSummary()
	st := &stats{sum: map[string]*kubeletstats.Summary{"node-a": sum}}
	other := testPod("node-b")
	other.Name = "elsewhere"
	s := New(Config{NodeName: "node-a"}, (&inv{}).setPods(testPod("node-a"), other), owners{}, st, nil)
	if req := s.Sample(context.Background(), time.Now()); len(req.GetUsage()) != 2 {
		t.Fatalf("rows = %d", len(req.GetUsage()))
	}
	if len(st.calls) != 1 || st.calls[0] != "node-a" {
		t.Fatalf("summary calls = %v, want only node-a", st.calls)
	}
	// Same name, new UID (StatefulSet pod recreated): stats belong to
	// the old pod.
	ps := sum.Pods[kubeletstats.PodKey{Namespace: "shop", Name: "checkout-abc"}]
	ps.UID = "old"
	sum.Pods[kubeletstats.PodKey{Namespace: "shop", Name: "checkout-abc"}] = ps
	if req := s.Sample(context.Background(), time.Now()); req != nil {
		t.Fatalf("stale incarnation reported: %v", req)
	}
}

func TestSampleDegradesWhenStatsFail(t *testing.T) {
	s := New(Config{NodeName: "node-a", Logger: slog.New(slog.DiscardHandler)}, (&inv{}).setPods(testPod("node-a")), owners{}, &stats{err: errors.New("403")}, nil)
	if req := s.Sample(context.Background(), time.Now()); req != nil {
		t.Fatalf("no stats must mean no rows, got %v", req)
	}
}

func TestRunEmits(t *testing.T) {
	st := &stats{sum: map[string]*kubeletstats.Summary{"node-a": testSummary()}}
	got := make(chan *kuberov1.IngestUsageRequest, 4)
	s := New(Config{NodeName: "node-a", Interval: 5 * time.Millisecond}, (&inv{}).setPods(testPod("node-a")), owners{}, st,
		func(r *kuberov1.IngestUsageRequest) { got <- r })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)
	select {
	case r := <-got:
		if len(r.Usage) != 2 {
			t.Fatalf("rows = %d", len(r.Usage))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sampler did not emit")
	}
}

func TestStaticPodStatsJoinByConfigHash(t *testing.T) {
	sum := testSummary()
	ps := sum.Pods[kubeletstats.PodKey{Namespace: "shop", Name: "checkout-abc"}]
	ps.UID = "confighash0123"
	sum.Pods[kubeletstats.PodKey{Namespace: "shop", Name: "checkout-abc"}] = ps
	p := testPod("node-a")
	p.Annotations = map[string]string{kube.AnnotationConfigHash: "confighash0123"}
	s := New(Config{NodeName: "node-a"}, (&inv{}).setPods(p), owners{}, &stats{sum: map[string]*kubeletstats.Summary{"node-a": sum}}, nil)
	if req := s.Sample(context.Background(), time.Now()); len(req.GetUsage()) != 2 {
		t.Fatalf("static pod usage rows = %d", len(req.GetUsage()))
	}
}
