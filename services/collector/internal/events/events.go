// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package events turns Kubernetes health signals into ClusterEvents.
//
// Sources and who runs them:
//
//   - Container status (every collector, node-local, every scan):
//     restarts by restartCount diff ("restarted", or "oom_killed" when
//     the last termination was OOMKilled), CrashLoopBackOff
//     ("crash_loop"), ImagePullBackOff / ErrImagePull
//     ("image_pull_backoff"), OOMKilled terminations of containers that
//     won't restart, and container configuration errors ("warning").
//   - Pending pods (leader only): PodScheduled=False/Unschedulable →
//     "unschedulable" with the resources the pod asks for, the input to
//     the control plane's capacity-demand view. Re-emitted every
//     pendingReemit while the pod stays pending, because the control
//     plane reads a trailing window.
//   - Warning events (leader only): list + watch of core/v1 Events with
//     type=Warning, classified by reason (Classify).
//   - Node conditions (leader only): Ready≠True → "node_not_ready",
//     Memory/Disk/PID pressure → "node_pressure".
//
// Overlap policy: kubelet's BackOff / Failed(image) / FailedScheduling
// Warning events describe the same occurrences the status and pending
// paths already report with better precision (dedupe per restart,
// requested resources), and NodeNotReady is covered by node conditions.
// Those Warning events are classified but not re-emitted
// (SuppressedByOtherSource), so every occurrence is counted once.
// Node-level OOM events (node-problem-detector OOMKilling, kubelet
// SystemOOM) have no pod and are kept.
package events

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

// Kinds (telemetry.proto ClusterEvent.kind).
const (
	KindOOMKilled        = "oom_killed"
	KindCrashLoop        = "crash_loop"
	KindImagePullBackoff = "image_pull_backoff"
	KindUnschedulable    = "unschedulable"
	KindEvicted          = "evicted"
	KindNodeNotReady     = "node_not_ready"
	KindNodePressure     = "node_pressure"
	KindRestarted        = "restarted"
	KindWarning          = "warning"
)

// Severities.
const (
	SeverityInfo     = "info"
	SeverityWarn     = "warn"
	SeverityCritical = "critical"
)

// maxMessage bounds free text copied from Kubernetes (termination
// messages can be 4 KiB, event messages longer).
const maxMessage = 2048

// Classify maps a Kubernetes Warning event to a kind and severity.
func Classify(reason, message string) (kind, severity string) {
	msg := strings.ToLower(message)
	switch reason {
	case "OOMKilling", "SystemOOM":
		return KindOOMKilled, SeverityWarn
	case "BackOff":
		if strings.Contains(msg, "image") || strings.Contains(msg, "pull") {
			return KindImagePullBackoff, SeverityWarn
		}
		return KindCrashLoop, SeverityWarn
	case "Failed":
		if strings.Contains(msg, "image") || strings.Contains(msg, "pull") {
			return KindImagePullBackoff, SeverityWarn
		}
		return KindWarning, SeverityWarn
	case "ErrImagePull", "ImagePullBackOff":
		return KindImagePullBackoff, SeverityWarn
	case "Evicted":
		return KindEvicted, SeverityWarn
	case "EvictionThresholdMet":
		return KindNodePressure, SeverityWarn
	case "NodeNotReady":
		return KindNodeNotReady, SeverityCritical
	case "FailedScheduling":
		return KindUnschedulable, SeverityWarn
	}
	return KindWarning, SeverityWarn
}

// SuppressedByOtherSource reports whether a classified Warning event is
// already reported by a more precise source (see the package comment).
func SuppressedByOtherSource(kind, involvedKind string) bool {
	switch kind {
	case KindCrashLoop, KindImagePullBackoff, KindUnschedulable:
		return involvedKind == "Pod"
	case KindNodeNotReady:
		return true
	}
	return false
}

func cloneEvent(e *kuberov1.ClusterEvent) *kuberov1.ClusterEvent {
	return proto.Clone(e).(*kuberov1.ClusterEvent)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// Don't split a UTF-8 sequence.
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

// ── batching ──────────────────────────────────────────────────────────

// Batcher collects events from every source and flushes them as one
// IngestEventsRequest every interval (or when maxBatch is reached). Its
// buffer is bounded: beyond maxBuffer the oldest events are dropped and
// counted, so an event storm can't grow the agent.
type Batcher struct {
	clusterID string
	interval  time.Duration
	maxBatch  int
	maxBuffer int
	emit      func(*kuberov1.IngestEventsRequest)

	mu   sync.Mutex
	buf  []*kuberov1.ClusterEvent
	kick chan struct{}
}

// NewBatcher builds a Batcher; emit receives each request.
func NewBatcher(clusterID string, interval time.Duration, emit func(*kuberov1.IngestEventsRequest)) *Batcher {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &Batcher{clusterID: clusterID, interval: interval, maxBatch: 500, maxBuffer: 10_000, emit: emit, kick: make(chan struct{}, 1)}
}

// Add queues events (source is a metrics label: status | k8s | pending | node).
func (b *Batcher) Add(source string, evs ...*kuberov1.ClusterEvent) {
	if len(evs) == 0 {
		return
	}
	b.mu.Lock()
	for _, e := range evs {
		metrics.EventsEmitted.With(e.GetKind(), source).Inc()
	}
	b.buf = append(b.buf, evs...)
	if over := len(b.buf) - b.maxBuffer; over > 0 {
		b.buf = append(b.buf[:0:0], b.buf[over:]...)
		metrics.ItemsDropped.With("events", "buffer_full").Add(float64(over))
	}
	full := len(b.buf) >= b.maxBatch
	b.mu.Unlock()
	if full {
		select {
		case b.kick <- struct{}{}:
		default:
		}
	}
}

// Run flushes periodically until ctx ends, then flushes once more.
func (b *Batcher) Run(ctx context.Context) {
	t := time.NewTicker(b.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			b.Flush()
			return
		case <-t.C:
		case <-b.kick:
		}
		b.Flush()
	}
}

// Flush emits everything buffered, in requests of at most maxBatch.
func (b *Batcher) Flush() {
	b.mu.Lock()
	buf := b.buf
	b.buf = nil
	b.mu.Unlock()
	for len(buf) > 0 {
		n := min(len(buf), b.maxBatch)
		if b.emit != nil {
			b.emit(&kuberov1.IngestEventsRequest{ClusterId: b.clusterID, Events: buf[:n]})
		}
		buf = buf[n:]
	}
}
