// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package ebpf is the collector's kernel telemetry: per-flow L3/L4
// byte accounting (cgroup_skb on the root cgroup v2), TCP retransmits
// (tcp:tcp_retransmit_skb), and whole-node CPU profiling (perf_event
// sampling with kernel + user stacks). Linux only; Start returns
// ErrUnsupported everywhere else and whenever the kernel or the
// container's privileges don't allow loading programs — callers log
// that once and carry on without kernel signals.
//
// The package owns loading, map draining and translation into the
// telemetry protos. Everything cluster-aware (which pod owns an IP,
// which pod a cgroup belongs to) comes in through Resolver, which the
// collector implements from its pod / service / node caches.
//
// # How it works
//
// Netflow: two cgroup_skb programs (egress, ingress) are multi-attached
// to the cgroup v2 root. cgroup hooks follow sockets, not network
// namespaces, so they see every inet socket on the node, in every pod,
// and never drop anything. They count bytes and packets per (family,
// protocol, src, dst, lower port, direction) in an LRU hash that is
// drained with batch lookup-and-delete every FlushInterval; rows are
// then attributed with Resolver.LookupIP and aggregated per endpoint
// pair. A tcp_retransmit_skb tracepoint program counts retransmits
// under the same key and they are joined onto the sender's egress row.
//
// Profiler: a cpu-clock perf event per online CPU samples every task at
// ProfileHz and records (cgroup id, pid, user stack, kernel stack). Maps
// are double-buffered so a drain never races the sampler. Cgroup ids
// resolve to pod UID + container ID through an index of the kubepods
// cgroup tree, then to a PodRef via Resolver.LookupContainer. Kernel
// frames are named from /proc/kallsyms; user frames from the ELF
// behind each mapping (.symtab, else a Go binary's .gopclntab, else
// .dynsym) and, for JIT code, from /tmp/perf-<pid>.map.
//
// No CO-RE: the programs use only stable UAPI contexts, so they need no
// kernel BTF (Docker Desktop's LinuxKit kernel, for one, has none). The
// objects are compiled ahead of time by gen.sh (bpf2go, clang 19,
// -target bpfel) and embedded.
//
// # Requirements
//
//   - Linux 5.8+ recommended. Older kernels degrade: batch drains need
//     5.6 (per-key fallback below), bpf_link cgroup attach 5.7 (legacy
//     multi-attach below), bpf_link for perf events 5.15 (ioctl below);
//     bpf_get_current_cgroup_id needs 4.18 and bpf_probe_read_kernel 5.5.
//   - cgroup v2 at Config.CgroupRoot: the host's hierarchy (hostPath
//     /sys/fs/cgroup; read-only is enough).
//   - Root with CAP_BPF, CAP_PERFMON and CAP_NET_ADMIN (or CAP_SYS_ADMIN;
//     privileged is simplest), CAP_SYS_ADMIN for /proc/<pid>/map_files,
//     CAP_SYS_PTRACE to read other processes' maps, and CAP_SYSLOG (or
//     kptr_restrict=0) for kallsyms addresses. Kernels before 5.11 also
//     need RLIMIT_MEMLOCK raised (done here; needs CAP_SYS_RESOURCE).
//   - Profiler: the host PID namespace (hostPID: true).
//   - Retransmits: tracefs at /sys/kernel/tracing or
//     /sys/kernel/debug/tracing; without it flows still work and report
//     zero retransmits.
//   - Kernel memory, charged to the collector's cgroup on 5.11+ (fdinfo
//     memlock on Linux 6.10): 15 MiB for the flow table (131072 entries,
//     ~120 B each; see FlowMapEntriesEnv), 1.8 MiB for retransmits and
//     2 x 9.7 MiB for the profiler's stack + count maps: ~36 MiB total.
//
// # Limitations
//
//   - Flows cover socket traffic only: packets a node merely forwards
//     (NodePort / hostPort DNAT to another node, routing) never reach a
//     local socket and are not seen there; they are seen on the nodes
//     whose sockets send and receive them. Bytes are L3 lengths at the
//     socket layer, one IP+TCP header per GSO/GRO super-packet. Port is
//     the lower of the two ports, which misattributes the rare server
//     listening above its clients' ephemeral range. Under heavy peer
//     churn the LRU table evicts cold entries between drains (reported
//     in Counters.MapFullEvents).
//   - User stacks are unwound with frame pointers. Go, the JVM with
//     -XX:+PreserveFramePointer and Rust / C / C++ built with frame
//     pointers unwind fully; code built with -fomit-frame-pointer
//     (common in distro C libraries) truncates at the first such frame.
//     There is no DWARF unwinding. On arm64 a leaf function that sets up
//     no frame record hides its immediate caller.
//   - JIT code (Node.js, JVM, .NET, Python 3.12+) is only named when the
//     runtime writes a perf map (--perf-basic-prof, perf-map-agent,
//     DOTNET_PerfMapEnabled=1, -X perf); otherwise it is [unknown].
//   - Stacks deeper than 127 frames are cut. Processes that exit before
//     the drain lose their user symbols. C++ names stay mangled. CPUs
//     brought online after Start are not sampled. A stack the kernel
//     cannot store (bucket collision, map full) keeps its CPU time under
//     an [unknown] frame (Counters.StacksLost).
//   - Samples are attributed only to Kubernetes containers
//     (Resolver.LookupContainer); system daemons and the kubelet are
//     dropped (Counters.SamplesUnattributed).
//
// Emitted batches share FlowEndpoint messages between rows; consumers
// must treat them as read-only. Start is meant to be called once per
// process; Stats reports process-wide totals.
package ebpf

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// ErrUnsupported means kernel telemetry cannot run here (non-Linux,
// no cgroup v2, a kernel that rejects the programs, or insufficient
// privileges).
var ErrUnsupported = errors.New("ebpf: unsupported on this host")

// Resolver attributes kernel-level identities to Kubernetes objects.
// Implementations must be safe for concurrent use and cheap: they are
// called for every drained flow key and every sampled stack.
type Resolver interface {
	// LookupIP classifies an address seen on the wire. Unknown
	// addresses must come back as Kind "external" with Name set to the
	// address (never nil / empty).
	LookupIP(ip netip.Addr) *kuberov1.FlowEndpoint
	// LookupContainer maps the pod UID and container ID parsed from a
	// cgroup path to the owning pod + container. ok=false for cgroups
	// that are not Kubernetes containers (system services, the
	// kubelet, …) — those samples are dropped. podUID is lower-case
	// with dashes (a static pod's is its 32-hex config hash);
	// containerID is the bare lower-case 64-hex runtime ID, without the
	// "containerd://" style prefix of the pod status, and empty for
	// processes directly in the pod-level cgroup.
	LookupContainer(podUID, containerID string) (ref *kuberov1.PodRef, ok bool)
	// ServiceName is the profile "service" label for a pod: the
	// kubehero.io/service annotation, else the workload name.
	ServiceName(ref *kuberov1.PodRef) string
}

// Config selects the programs to run and where their output goes.
type Config struct {
	Netflow  bool // cgroup_skb flow accounting + TCP retransmits
	Profiler bool // perf_event CPU sampling

	// CgroupRoot is the cgroup v2 mount the flow programs attach to.
	// Default "/sys/fs/cgroup".
	CgroupRoot string
	// FlushInterval is how often maps are drained and emitted.
	// Default 15s.
	FlushInterval time.Duration
	// ProfileHz is the per-CPU sampling frequency. Default 49.
	ProfileHz int
	// IncludeLoopback keeps 127.0.0.0/8 and ::1 traffic (tests only).
	IncludeLoopback bool

	Resolver Resolver
	Logger   *slog.Logger

	// EmitFlows / EmitProfiles receive translated batches. They are
	// called from the drain loops; errors are logged, never fatal.
	EmitFlows    func(ctx context.Context, flows []*kuberov1.Flow) error
	EmitProfiles func(ctx context.Context, profiles []*kuberov1.Profile) error
}

// Start loads and attaches the enabled programs and runs their drain
// loops until ctx is cancelled, then detaches everything. It returns
// once the programs are attached (loops keep running in the
// background) or with an error wrapping ErrUnsupported.
func Start(ctx context.Context, cfg Config) error {
	return start(ctx, cfg)
}
