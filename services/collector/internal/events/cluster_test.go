// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package events

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

type sink struct {
	mu  sync.Mutex
	evs []*kuberov1.ClusterEvent
	src []string
}

func (s *sink) add(source string, evs ...*kuberov1.ClusterEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range evs {
		s.evs = append(s.evs, e)
		s.src = append(s.src, source)
	}
}

func (s *sink) take() []*kuberov1.ClusterEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.evs
	s.evs, s.src = nil, nil
	return out
}

func newWatcher(cs *fake.Clientset, s *sink) *ClusterWatcher {
	return NewClusterWatcher(ClusterConfig{Logger: slog.New(slog.DiscardHandler)}, cs, owners{}, s.add)
}

func pendingPod(name string, unschedulable bool) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ml", Name: name, UID: types.UID("u-" + name),
			CreationTimestamp: metav1.NewTime(time.Now().Add(-90 * time.Second))},
		Spec: corev1.PodSpec{
			NodeSelector: map[string]string{"karpenter.sh/nodepool": "gpu-a100"},
			Containers: []corev1.Container{{Name: "train", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("8"), corev1.ResourceMemory: resource.MustParse("64Gi")},
				Limits:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")},
			}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	if unschedulable {
		p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: "0/12 nodes are available: 12 Insufficient nvidia.com/gpu."}}
	}
	return p
}

func TestScanPending(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPod("train-0", true), pendingPod("creating", false))
	s := &sink{}
	w := newWatcher(cs, s)
	now := time.Now()
	w.now = func() time.Time { return now }

	w.ScanPending(context.Background())
	evs := s.take()
	if len(evs) != 1 {
		t.Fatalf("events = %v", kinds(evs))
	}
	e := evs[0]
	a := e.Attributes
	if e.Kind != KindUnschedulable || a["cpu_millicores"] != "8000" || a["mem_bytes"] != "68719476736" || a["gpu"] != "2" ||
		a["workload"] != "api" || a["workload_kind"] != "Deployment" || a["nodepool"] != "gpu-a100" ||
		a["message"] == "" || a["age_sec"] == "" || a["age_sec"] == "0" {
		t.Fatalf("unschedulable event = %+v", e)
	}
	if e.GetSource().GetPod() != "train-0" || e.GetSource().GetNamespace() != "ml" {
		t.Fatalf("source = %+v", e.GetSource())
	}
	// Still pending within the re-emit window: quiet.
	w.ScanPending(context.Background())
	if evs := s.take(); len(evs) != 0 {
		t.Fatalf("re-emitted too early: %v", kinds(evs))
	}
	// After the window: reported again so the control plane's trailing
	// window keeps seeing the demand.
	now = now.Add(6 * time.Minute)
	w.ScanPending(context.Background())
	if evs := s.take(); len(evs) != 1 {
		t.Fatalf("re-emit after window = %v", kinds(evs))
	}
	// Scheduled → forgotten.
	if err := cs.CoreV1().Pods("ml").Delete(context.Background(), "train-0", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	w.ScanPending(context.Background())
	if len(w.pending) != 0 {
		t.Fatalf("pending state not cleaned: %v", w.pending)
	}
}

func TestScanNodes(t *testing.T) {
	s := &sink{}
	w := newWatcher(fake.NewSimpleClientset(), s)
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	w.nodes = store
	t0 := metav1.NewTime(time.Now().Add(-time.Minute))
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{"topology.kubernetes.io/zone": "z1"}},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeReady, Status: corev1.ConditionUnknown, Reason: "NodeStatusUnknown", Message: "Kubelet stopped posting node status.", LastTransitionTime: t0},
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue, Reason: "KubeletHasInsufficientMemory", LastTransitionTime: t0},
			{Type: corev1.NodeDiskPressure, Status: corev1.ConditionFalse, LastTransitionTime: t0},
		}}}
	_ = store.Add(node)
	w.ScanNodes()
	evs := s.take()
	if len(evs) != 2 || evs[0].Kind != KindNodeNotReady || evs[0].Severity != SeverityCritical || evs[1].Kind != KindNodePressure ||
		evs[1].Attributes["condition"] != "MemoryPressure" || evs[0].GetSource().GetNode() != "n1" || evs[0].GetSource().GetZone() != "z1" {
		t.Fatalf("node events = %+v", evs)
	}
	w.ScanNodes()
	if evs := s.take(); len(evs) != 0 {
		t.Fatalf("same episode re-reported: %v", kinds(evs))
	}
	// Recovered, then a new NotReady episode.
	n2 := node.DeepCopy()
	n2.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(time.Now())}}
	_ = store.Update(n2)
	w.ScanNodes()
	if evs := s.take(); len(evs) != 1 || evs[0].Kind != KindNodeNotReady {
		t.Fatalf("new episode = %v", kinds(evs))
	}
}

func warningEvent(uid, reason, msg string, kind, name string, count int32, last time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: "shop", Name: uid, UID: types.UID(uid), ResourceVersion: "1"},
		Type:           corev1.EventTypeWarning,
		Reason:         reason,
		Message:        msg,
		Count:          count,
		FirstTimestamp: metav1.NewTime(last.Add(-time.Duration(count) * time.Second)),
		LastTimestamp:  metav1.NewTime(last),
		InvolvedObject: corev1.ObjectReference{Kind: kind, Namespace: "shop", Name: name, UID: types.UID("obj-" + name), FieldPath: "spec.containers{app}"},
		Source:         corev1.EventSource{Component: "kubelet", Host: "node-a"},
	}
}

func TestObserveWarnings(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", UID: "obj-api-1"}})
	s := &sink{}
	w := newWatcher(cs, s)
	now := time.Now()
	w.now = func() time.Time { return now }
	cutoff := now.Add(-5 * time.Minute)
	ctx := context.Background()

	// History before the cutoff is baselined silently.
	w.observe(ctx, warningEvent("e-old", "Evicted", "The node was low on resource: memory.", "Pod", "api-1", 1, now.Add(-time.Hour)), cutoff)
	// Recent eviction: reported, attributed through a pod GET.
	w.observe(ctx, warningEvent("e-evict", "Evicted", "The node was low on resource: memory.", "Pod", "api-1", 1, now.Add(-time.Minute)), cutoff)
	// Crash-loop BackOff on a pod: covered by the status path.
	w.observe(ctx, warningEvent("e-backoff", "BackOff", "Back-off restarting failed container", "Pod", "api-1", 7, now), cutoff)
	// Node-level OOM from node-problem-detector: kept.
	w.observe(ctx, warningEvent("e-oom", "OOMKilling", "Out of memory: Killed process 4242 (java)", "Node", "node-a", 1, now), cutoff)
	// Normal events never count.
	normal := warningEvent("e-normal", "Pulled", "ok", "Pod", "api-1", 1, now)
	normal.Type = corev1.EventTypeNormal
	w.observe(ctx, normal, cutoff)

	evs := s.take()
	if len(evs) != 2 || evs[0].Kind != KindEvicted || evs[1].Kind != KindOOMKilled {
		t.Fatalf("emitted %v", kinds(evs))
	}
	ev := evs[0]
	if ev.GetSource().GetWorkload() != "api" || ev.GetSource().GetContainer() != "app" || ev.Reason != "Evicted" ||
		ev.Attributes["reporting_component"] != "kubelet" || ev.Count != 1 || ev.TsUnixMs != now.Add(-time.Minute).UnixMilli() {
		t.Fatalf("evicted event = %+v", ev)
	}
	if evs[1].GetSource().GetNode() != "node-a" || evs[1].GetSource().GetPod() != "" {
		t.Fatalf("node OOM source = %+v", evs[1].GetSource())
	}

	// A chatty aggregated warning: count bumps are throttled, then
	// reported as one aggregated delta.
	w.observe(ctx, warningEvent("e-probe", "Unhealthy", "Readiness probe failed", "Pod", "api-1", 1, now), time.Time{})
	if evs := s.take(); len(evs) != 1 || evs[0].Kind != KindWarning || evs[0].Count != 1 {
		t.Fatalf("first probe warning = %v", evs)
	}
	for c := int32(2); c <= 10; c++ {
		w.observe(ctx, warningEvent("e-probe", "Unhealthy", "Readiness probe failed", "Pod", "api-1", c, now), time.Time{})
	}
	if evs := s.take(); len(evs) != 0 {
		t.Fatalf("throttle leaked %d events", len(evs))
	}
	now = now.Add(6 * time.Minute)
	w.flushWarnings()
	evs = s.take()
	if len(evs) != 1 || evs[0].Count != 9 {
		t.Fatalf("aggregated re-report = %+v", evs)
	}
}

// First sight of a long-aggregated event after a (re)list only reports
// the recent repeat, not its whole history.
func TestObserveAggregatedHistoryOnList(t *testing.T) {
	s := &sink{}
	w := newWatcher(fake.NewSimpleClientset(), s)
	now := time.Now()
	w.now = func() time.Time { return now }
	ev := warningEvent("e1", "FailedMount", "MountVolume.SetUp failed", "Pod", "gone", 400, now)
	ev.FirstTimestamp = metav1.NewTime(now.Add(-3 * time.Hour))
	w.observe(context.Background(), ev, now.Add(-5*time.Minute))
	if evs := s.take(); len(evs) != 1 || evs[0].Count != 1 || evs[0].GetSource().GetPod() != "gone" {
		t.Fatalf("aggregated first sight = %+v", evs)
	}
}

func TestWatchWarningsEndToEnd(t *testing.T) {
	cs := fake.NewSimpleClientset()
	s := &sink{}
	w := newWatcher(cs, s)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { w.watchWarnings(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Create repeatedly until the watch is established and sees one.
		_, _ = cs.CoreV1().Events("shop").Create(ctx, warningEvent("e-"+time.Now().Format("150405.000000"), "Evicted", "low memory", "Pod", "x", 1, time.Now()), metav1.CreateOptions{})
		time.Sleep(50 * time.Millisecond)
		s.mu.Lock()
		n := len(s.evs)
		s.mu.Unlock()
		if n > 0 {
			cancel()
			<-done
			return
		}
	}
	t.Fatal("watch never delivered a Warning event")
}

func TestClassify(t *testing.T) {
	cases := []struct{ reason, msg, kind string }{
		{"OOMKilling", "Out of memory: Killed process 1", KindOOMKilled},
		{"SystemOOM", "System OOM encountered", KindOOMKilled},
		{"BackOff", "Back-off restarting failed container app", KindCrashLoop},
		{"BackOff", "Back-off pulling image \"x:v1\"", KindImagePullBackoff},
		{"Failed", "Failed to pull image \"x\": not found", KindImagePullBackoff},
		{"Failed", "Error: container create failed", KindWarning},
		{"Evicted", "The node was low on resource", KindEvicted},
		{"EvictionThresholdMet", "Attempting to reclaim memory", KindNodePressure},
		{"NodeNotReady", "Node is not ready", KindNodeNotReady},
		{"FailedScheduling", "0/3 nodes are available", KindUnschedulable},
		{"Unhealthy", "Liveness probe failed", KindWarning},
	}
	for _, c := range cases {
		if k, _ := Classify(c.reason, c.msg); k != c.kind {
			t.Errorf("Classify(%q, %q) = %q, want %q", c.reason, c.msg, k, c.kind)
		}
	}
	if !SuppressedByOtherSource(KindCrashLoop, "Pod") || SuppressedByOtherSource(KindOOMKilled, "Node") ||
		!SuppressedByOtherSource(KindUnschedulable, "Pod") || !SuppressedByOtherSource(KindNodeNotReady, "Node") ||
		SuppressedByOtherSource(KindEvicted, "Pod") {
		t.Fatal("suppression policy changed")
	}
}

func TestBatcher(t *testing.T) {
	var mu sync.Mutex
	var reqs []*kuberov1.IngestEventsRequest
	b := NewBatcher("c1", time.Hour, func(r *kuberov1.IngestEventsRequest) {
		mu.Lock()
		reqs = append(reqs, r)
		mu.Unlock()
	})
	b.maxBatch, b.maxBuffer = 3, 5
	for range 7 {
		b.Add("status", &kuberov1.ClusterEvent{Kind: KindRestarted})
	}
	b.Flush()
	mu.Lock()
	defer mu.Unlock()
	// Buffer bound 5: the 2 oldest dropped; 5 flushed as 3 + 2.
	if len(reqs) != 2 || len(reqs[0].Events) != 3 || len(reqs[1].Events) != 2 || reqs[0].ClusterId != "c1" {
		t.Fatalf("requests = %d", len(reqs))
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("héllo", 2); got != "h…" {
		t.Fatalf("truncate split a rune: %q", got)
	}
	if truncate("abc", 5) != "abc" {
		t.Fatal("short strings unchanged")
	}
}

// Without nodes RBAC the node informer never syncs; pending pods must
// still be reported. Without events RBAC the watcher backs off quietly.
func TestClusterWatcherDegradesWithoutRBAC(t *testing.T) {
	cs := fake.NewSimpleClientset(pendingPod("train-0", true))
	forbid := func(resource string) {
		cs.PrependReactor("list", resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", nil)
		})
	}
	forbid("nodes")
	forbid("events")
	s := &sink{}
	w := NewClusterWatcher(ClusterConfig{Interval: 20 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)}, cs, owners{}, s.add)
	w.nodeSyncWait = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.evs)
		s.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if evs := s.take(); len(evs) != 1 || evs[0].Kind != KindUnschedulable {
		t.Fatalf("pending scan without node/event RBAC = %v", kinds(evs))
	}
}
