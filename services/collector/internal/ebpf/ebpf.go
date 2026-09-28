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
// missing BTF / cgroup v2, or insufficient privileges).
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
	// kubelet, …) — those samples are dropped.
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
