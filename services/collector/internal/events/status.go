// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package events

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
)

// Inventory is what the detector reads from the node-local cache.
type Inventory interface {
	Pods() []*corev1.Pod
	Node(name string) *corev1.Node
}

// Owners resolves pods to workloads.
type Owners interface {
	Resolve(ctx context.Context, p *corev1.Pod) kube.Workload
}

// maxDedupe bounds the emitted-occurrence memory; far above what one
// node's pods can produce between restarts of the collector.
const maxDedupe = 50_000

// StatusDetector finds container health events in the node-local pod
// cache by diffing container statuses between scans.
type StatusDetector struct {
	inv    Inventory
	owners Owners

	// restarts is the restartCount baseline per container. A container
	// seen for the first time is baselined without events — its past
	// restarts happened before we were watching and would otherwise be
	// re-reported on every collector restart.
	restarts map[containerKey]int32
	// emitted remembers occurrences already reported, keyed by
	// (container, kind, restartCount) — so a crash loop reports once
	// per restart, not once per scan.
	emitted map[occurrence]struct{}
}

type containerKey struct {
	uid  types.UID
	name string
}

type occurrence struct {
	containerKey
	kind    string
	restart int32
}

// NewStatusDetector builds a detector.
func NewStatusDetector(inv Inventory, owners Owners) *StatusDetector {
	return &StatusDetector{inv: inv, owners: owners, restarts: map[containerKey]int32{}, emitted: map[occurrence]struct{}{}}
}

// Detect scans the cache once and returns new events.
func (d *StatusDetector) Detect(ctx context.Context, now time.Time) []*kuberov1.ClusterEvent {
	var out []*kuberov1.ClusterEvent
	live := map[containerKey]bool{}
	for _, p := range d.inv.Pods() {
		var ref *kuberov1.PodRef
		podRef := func() *kuberov1.PodRef {
			if ref == nil {
				w := kube.Workload{Name: p.Name, Kind: "Pod"}
				if d.owners != nil {
					w = d.owners.Resolve(ctx, p)
				}
				ref = kube.PodRefFor(p, d.inv.Node(p.Spec.NodeName), w)
			}
			return ref
		}
		statuses := make([]corev1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
		statuses = append(statuses, p.Status.InitContainerStatuses...)
		statuses = append(statuses, p.Status.ContainerStatuses...)
		for i := range statuses {
			st := &statuses[i]
			key := containerKey{uid: p.UID, name: st.Name}
			live[key] = true
			out = append(out, d.container(now, key, st, podRef)...)
		}
	}
	// Forget containers that are gone.
	for k := range d.restarts {
		if !live[k] {
			delete(d.restarts, k)
		}
	}
	for o := range d.emitted {
		if !live[o.containerKey] {
			delete(d.emitted, o)
		}
	}
	return out
}

func (d *StatusDetector) container(now time.Time, key containerKey, st *corev1.ContainerStatus, podRef func() *kuberov1.PodRef) []*kuberov1.ClusterEvent {
	var out []*kuberov1.ClusterEvent
	mk := func(kind, severity, reason, message string, count int32, attrs map[string]string) *kuberov1.ClusterEvent {
		src := clonePodRef(podRef())
		src.Container = st.Name
		return &kuberov1.ClusterEvent{
			TsUnixMs: now.UnixMilli(), Kind: kind, Severity: severity, Source: src,
			Reason: reason, Message: truncate(message, maxMessage), Attributes: attrs, Count: count,
		}
	}

	// Restarts: diff against the baseline.
	prev, known := d.restarts[key]
	d.restarts[key] = st.RestartCount
	if known && st.RestartCount > prev {
		delta := st.RestartCount - prev
		term := st.LastTerminationState.Terminated
		kind, severity, reason := KindRestarted, SeverityInfo, ""
		attrs := map[string]string{"restart_count": strconv.Itoa(int(st.RestartCount))}
		msg := "container restarted"
		if term != nil {
			reason = term.Reason
			attrs["exit_code"] = strconv.Itoa(int(term.ExitCode))
			if term.Signal != 0 {
				attrs["signal"] = strconv.Itoa(int(term.Signal))
			}
			if !term.FinishedAt.IsZero() {
				attrs["finished_at"] = term.FinishedAt.UTC().Format(time.RFC3339)
			}
			msg = "container restarted after exit code " + strconv.Itoa(int(term.ExitCode))
			if term.Message != "" {
				msg += ": " + term.Message
			}
			if term.Reason == "OOMKilled" {
				kind, severity = KindOOMKilled, SeverityWarn
				msg = "container OOM-killed and restarted"
			}
		}
		if delta > 1 {
			attrs["restarts_since_last_scan"] = strconv.Itoa(int(delta))
		}
		// The termination belongs to the previous run (restartCount-1);
		// an OOMKilled terminated state reported before the restart used
		// the same key, so the OOM is counted once.
		occ := occurrence{containerKey: key, kind: kind, restart: st.RestartCount - 1}
		if d.remember(occ) {
			out = append(out, mk(kind, severity, reason, msg, delta, attrs))
		}
	}

	// Current state.
	switch {
	case st.State.Waiting != nil:
		w := st.State.Waiting
		kind, severity := waitingKind(w.Reason)
		if kind == "" {
			break
		}
		attrs := map[string]string{"restart_count": strconv.Itoa(int(st.RestartCount))}
		if kind == KindImagePullBackoff && st.Image != "" {
			attrs["image"] = st.Image
		}
		if d.remember(occurrence{containerKey: key, kind: kind, restart: st.RestartCount}) {
			out = append(out, mk(kind, severity, w.Reason, w.Message, 1, attrs))
		}
	case st.State.Terminated != nil && st.State.Terminated.Reason == "OOMKilled":
		// Containers that won't restart (Jobs with restartPolicy Never,
		// or caught between termination and restart) still OOMed.
		t := st.State.Terminated
		attrs := map[string]string{
			"restart_count": strconv.Itoa(int(st.RestartCount)),
			"exit_code":     strconv.Itoa(int(t.ExitCode)),
		}
		if d.remember(occurrence{containerKey: key, kind: KindOOMKilled, restart: st.RestartCount}) {
			out = append(out, mk(KindOOMKilled, SeverityWarn, t.Reason, "container OOM-killed", 1, attrs))
		}
	}
	return out
}

// remember records an occurrence and reports whether it is new.
func (d *StatusDetector) remember(o occurrence) bool {
	if _, ok := d.emitted[o]; ok {
		return false
	}
	if len(d.emitted) >= maxDedupe {
		// Pathological churn: start over rather than grow. Worst case a
		// few ongoing conditions are reported a second time.
		d.emitted = map[occurrence]struct{}{}
	}
	d.emitted[o] = struct{}{}
	return true
}

func waitingKind(reason string) (kind, severity string) {
	switch reason {
	case "CrashLoopBackOff":
		return KindCrashLoop, SeverityWarn
	case "ImagePullBackOff", "ErrImagePull":
		return KindImagePullBackoff, SeverityWarn
	case "CreateContainerConfigError", "CreateContainerError", "InvalidImageName", "RunContainerError", "ErrImageNeverPull":
		return KindWarning, SeverityWarn
	}
	return "", ""
}

func clonePodRef(r *kuberov1.PodRef) *kuberov1.PodRef {
	return proto.Clone(r).(*kuberov1.PodRef)
}
