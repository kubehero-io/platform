// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf

import (
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync/atomic"
)

// Counters is a snapshot of the kernel telemetry counters for the
// collector's /metrics endpoint. Counts are cumulative for the life of
// the process (Prometheus counters); the Attached fields are gauges.
type Counters struct {
	// NetflowAttached: the cgroup_skb flow programs are attached.
	NetflowAttached bool
	// RetransmitsAttached: the tcp_retransmit_skb tracepoint is attached
	// (optional part of Netflow; needs tracefs).
	RetransmitsAttached bool
	// ProfilerAttached: the perf_event sampler is attached to >= 1 CPU.
	ProfilerAttached bool

	// FlowsEmitted counts Flow rows accepted by EmitFlows.
	FlowsEmitted uint64
	// FlowEntriesDrained counts raw kernel flow-map entries read (before
	// aggregation into rows).
	FlowEntriesDrained uint64
	// ProfilesEmitted counts Profile messages accepted by EmitProfiles.
	ProfilesEmitted uint64
	// SamplesDrained counts CPU samples read from the kernel.
	SamplesDrained uint64
	// SamplesUnattributed counts samples dropped because their cgroup is
	// not a Kubernetes container known to the Resolver (system daemons,
	// the kubelet, containers that already exited).
	SamplesUnattributed uint64
	// StacksCapped counts unique stacks dropped by the per-drain cap.
	StacksCapped uint64
	// StacksLost counts samples whose stack the kernel could not record
	// (STACK_TRACE bucket collision or map full); their CPU time is kept
	// under an "[unknown]" frame.
	StacksLost uint64

	// DrainErrors counts failed map drains and failed emits.
	DrainErrors uint64
	// MapFullEvents counts kernel map updates that failed for lack of
	// space, plus flow drains that found the LRU flow table >= 90% full
	// (where eviction of live entries becomes likely).
	MapFullEvents uint64
}

// counters is the package's single set of counters: Start is called
// once per process, and a Prometheus scrape wants process totals.
var counters struct {
	netflowAttached     atomic.Bool
	retransmitsAttached atomic.Bool
	profilerAttached    atomic.Bool

	flowsEmitted        atomic.Uint64
	flowEntriesDrained  atomic.Uint64
	profilesEmitted     atomic.Uint64
	samplesDrained      atomic.Uint64
	samplesUnattributed atomic.Uint64
	stacksCapped        atomic.Uint64
	stacksLost          atomic.Uint64
	drainErrors         atomic.Uint64
	mapFullEvents       atomic.Uint64
}

// Stats returns the current counters. Safe for concurrent use.
func Stats() Counters {
	return Counters{
		NetflowAttached:     counters.netflowAttached.Load(),
		RetransmitsAttached: counters.retransmitsAttached.Load(),
		ProfilerAttached:    counters.profilerAttached.Load(),
		FlowsEmitted:        counters.flowsEmitted.Load(),
		FlowEntriesDrained:  counters.flowEntriesDrained.Load(),
		ProfilesEmitted:     counters.profilesEmitted.Load(),
		SamplesDrained:      counters.samplesDrained.Load(),
		SamplesUnattributed: counters.samplesUnattributed.Load(),
		StacksCapped:        counters.stacksCapped.Load(),
		StacksLost:          counters.stacksLost.Load(),
		DrainErrors:         counters.drainErrors.Load(),
		MapFullEvents:       counters.mapFullEvents.Load(),
	}
}

// deltaCounter turns a cumulative kernel counter (per-CPU error slots
// that are never reset) into increments for the package counters.
type deltaCounter struct{ last uint64 }

func (d *deltaCounter) delta(total uint64) uint64 {
	if total < d.last { // cannot happen for monotonic slots; be safe
		d.last = total
		return 0
	}
	n := total - d.last
	d.last = total
	return n
}

// guard runs one drain. The drains parse untrusted input (binaries from
// every container, procfs), so a bug there must cost one window of
// telemetry, not the node agent: panics are logged with their stack and
// counted as drain errors.
func guard(log *slog.Logger, what string, fn func()) {
	defer func() {
		if p := recover(); p != nil {
			counters.drainErrors.Add(1)
			log.Error("ebpf: drain panicked; window dropped", "drain", what, "panic", fmt.Sprint(p), "stack", string(debug.Stack()))
		}
	}()
	fn()
}
