// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package events

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
)

type inv struct {
	pods []*corev1.Pod
	node *corev1.Node
}

func (i *inv) Pods() []*corev1.Pod          { return i.pods }
func (i *inv) Node(string) *corev1.Node     { return i.node }
func (i *inv) Nodes() []*corev1.Node        { return []*corev1.Node{i.node} }
func (i *inv) set(p ...*corev1.Pod) *inv    { i.pods = p; return i }
func (i *inv) withNode(n *corev1.Node) *inv { i.node = n; return i }

type owners struct{}

func (owners) Resolve(context.Context, *corev1.Pod) kube.Workload {
	return kube.Workload{Name: "api", Kind: "Deployment"}
}

func pod(statuses ...corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "api-1", UID: "u1", Labels: map[string]string{"kubehero.io/team": "payments"}},
		Spec:       corev1.PodSpec{NodeName: "node-a"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: statuses},
	}
}

func running(name string, restarts int32, lastReason string, exit int32) corev1.ContainerStatus {
	st := corev1.ContainerStatus{Name: name, RestartCount: restarts, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
	if lastReason != "" {
		st.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: lastReason, ExitCode: exit, Signal: 9, Message: "boom"}
	}
	return st
}

func waiting(name string, restarts int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{Name: name, RestartCount: restarts, Image: "registry/app:v2",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: reason + " message"}}}
}

func kinds(evs []*kuberov1.ClusterEvent) []string {
	out := make([]string, len(evs))
	for i, e := range evs {
		out[i] = e.GetKind()
	}
	return out
}

func TestRestartDiffing(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"topology.kubernetes.io/zone": "z1"}}}
	in := (&inv{}).withNode(node).set(pod(running("app", 5, "Error", 1)))
	d := NewStatusDetector(in, owners{})
	now := time.Now()

	// First sight: 5 historical restarts are baselined, not reported.
	if evs := d.Detect(context.Background(), now); len(evs) != 0 {
		t.Fatalf("first sight emitted %v", kinds(evs))
	}
	in.set(pod(running("app", 6, "Error", 1)))
	evs := d.Detect(context.Background(), now)
	if len(evs) != 1 {
		t.Fatalf("restart events = %v", kinds(evs))
	}
	e := evs[0]
	if e.Kind != KindRestarted || e.Severity != SeverityInfo || e.Reason != "Error" || e.Count != 1 ||
		e.Attributes["exit_code"] != "1" || e.Attributes["restart_count"] != "6" || e.Attributes["signal"] != "9" {
		t.Fatalf("restart event = %+v", e)
	}
	src := e.GetSource()
	if src.GetContainer() != "app" || src.GetWorkload() != "api" || src.GetTeam() != "payments" || src.GetZone() != "z1" || src.GetPodUid() != "u1" {
		t.Fatalf("source = %+v", src)
	}
	// No change → nothing.
	if evs := d.Detect(context.Background(), now); len(evs) != 0 {
		t.Fatalf("steady state emitted %v", kinds(evs))
	}
	// Two restarts between scans with an OOM last: one event, count 2.
	in.set(pod(running("app", 8, "OOMKilled", 137)))
	evs = d.Detect(context.Background(), now)
	if len(evs) != 1 || evs[0].Kind != KindOOMKilled || evs[0].Severity != SeverityWarn || evs[0].Count != 2 ||
		evs[0].Attributes["restarts_since_last_scan"] != "2" {
		t.Fatalf("oom restart = %+v", evs)
	}
}

// The OOM seen in the terminated state and then again as lastState after
// the restart is one OOM.
func TestOOMCountedOnce(t *testing.T) {
	in := (&inv{}).set(pod(running("app", 2, "", 0)))
	d := NewStatusDetector(in, owners{})
	d.Detect(context.Background(), time.Now())

	term := corev1.ContainerStatus{Name: "app", RestartCount: 2,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}}}
	in.set(pod(term))
	evs := d.Detect(context.Background(), time.Now())
	if len(evs) != 1 || evs[0].Kind != KindOOMKilled {
		t.Fatalf("terminated OOM = %v", kinds(evs))
	}
	in.set(pod(running("app", 3, "OOMKilled", 137)))
	if evs := d.Detect(context.Background(), time.Now()); len(evs) != 0 {
		t.Fatalf("OOM double-counted after restart: %v", kinds(evs))
	}
	// A Job container that OOMs and never restarts is still reported.
	d2 := NewStatusDetector((&inv{}).set(pod(term)), owners{})
	if evs := d2.Detect(context.Background(), time.Now()); len(evs) != 1 || evs[0].Kind != KindOOMKilled {
		t.Fatalf("non-restarting OOM = %v", kinds(evs))
	}
}

func TestWaitingStates(t *testing.T) {
	in := (&inv{}).set(pod(waiting("app", 3, "CrashLoopBackOff"), waiting("sidecar", 0, "ImagePullBackOff"), waiting("cfg", 0, "CreateContainerConfigError"), waiting("new", 0, "ContainerCreating")))
	d := NewStatusDetector(in, owners{})
	evs := d.Detect(context.Background(), time.Now())
	got := map[string]*kuberov1.ClusterEvent{}
	for _, e := range evs {
		got[e.GetSource().GetContainer()] = e
	}
	if len(evs) != 3 || got["app"].Kind != KindCrashLoop || got["sidecar"].Kind != KindImagePullBackoff || got["cfg"].Kind != KindWarning {
		t.Fatalf("waiting events = %v", kinds(evs))
	}
	if got["sidecar"].Attributes["image"] != "registry/app:v2" || got["app"].Reason != "CrashLoopBackOff" || got["app"].Message == "" {
		t.Fatalf("event detail = %+v / %+v", got["sidecar"], got["app"])
	}
	// Same state next scan: deduped. ErrImagePull ↔ ImagePullBackOff
	// flapping is the same occurrence.
	in.set(pod(waiting("app", 3, "CrashLoopBackOff"), waiting("sidecar", 0, "ErrImagePull"), waiting("cfg", 0, "CreateContainerConfigError")))
	if evs := d.Detect(context.Background(), time.Now()); len(evs) != 0 {
		t.Fatalf("repeated waiting state emitted %v", kinds(evs))
	}
	// Next crash: a restart event plus a new crash_loop occurrence.
	in.set(pod(corev1.ContainerStatus{Name: "app", RestartCount: 4,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}}}))
	evs = d.Detect(context.Background(), time.Now())
	if len(evs) != 2 || evs[0].Kind != KindRestarted || evs[1].Kind != KindCrashLoop {
		t.Fatalf("next crash = %v", kinds(evs))
	}
}

func TestInitContainersAndCleanup(t *testing.T) {
	p := pod()
	p.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting("init-db", 1, "CrashLoopBackOff")}
	in := (&inv{}).set(p)
	d := NewStatusDetector(in, owners{})
	if evs := d.Detect(context.Background(), time.Now()); len(evs) != 1 || evs[0].GetSource().GetContainer() != "init-db" {
		t.Fatalf("init container events = %v", kinds(evs))
	}
	in.set()
	d.Detect(context.Background(), time.Now())
	if len(d.restarts) != 0 || len(d.emitted) != 0 {
		t.Fatalf("state not cleaned: %d restarts, %d emitted", len(d.restarts), len(d.emitted))
	}
}

func TestRestartTimestampAndInvalidUTF8(t *testing.T) {
	finished := time.Date(2026, 9, 28, 9, 59, 30, 0, time.UTC)
	st := running("app", 1, "Error", 1)
	st.LastTerminationState.Terminated.FinishedAt = metav1.NewTime(finished)
	st.LastTerminationState.Terminated.Message = "bad bytes \xff\xfe here"
	in := (&inv{}).set(pod(running("app", 0, "", 0)))
	d := NewStatusDetector(in, owners{})
	d.Detect(context.Background(), time.Now())
	in.set(pod(st))
	evs := d.Detect(context.Background(), time.Now())
	if len(evs) != 1 || evs[0].TsUnixMs != finished.UnixMilli() {
		t.Fatalf("restart event ts = %v, want finishedAt", evs)
	}
	if _, err := proto.Marshal(evs[0]); err != nil {
		t.Fatalf("event with a binary termination message must marshal: %v", err)
	}
}
