// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

package ebpf_test

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/collector/internal/ebpf"
)

// nodeResolver stands in for the collector's pod / service / node caches.
type nodeResolver struct{}

func (nodeResolver) LookupIP(ip netip.Addr) *kuberov1.FlowEndpoint {
	return &kuberov1.FlowEndpoint{Ip: ip.String(), Kind: "external", Name: ip.String()}
}

func (nodeResolver) LookupContainer(podUID, containerID string) (*kuberov1.PodRef, bool) {
	return nil, false // look the pod up by UID and the container by its bare ID
}

func (nodeResolver) ServiceName(ref *kuberov1.PodRef) string { return ref.GetWorkload() }

// The collector starts kernel telemetry once, treats ErrUnsupported as
// "no kernel signals on this node" and anything else as a bug, and
// exports Stats on /metrics.
func ExampleStart() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.Default()

	err := ebpf.Start(ctx, ebpf.Config{
		Netflow:       true,
		Profiler:      true,
		FlushInterval: 15 * time.Second,
		Resolver:      nodeResolver{},
		Logger:        log,
		EmitFlows: func(ctx context.Context, flows []*kuberov1.Flow) error {
			return nil // TelemetryService.IngestFlows
		},
		EmitProfiles: func(ctx context.Context, profiles []*kuberov1.Profile) error {
			return nil // TelemetryService.IngestProfiles
		},
	})
	switch {
	case errors.Is(err, ebpf.ErrUnsupported):
		log.Info("kernel telemetry unavailable on this node", "err", err)
	case err != nil:
		log.Error("kernel telemetry misconfigured", "err", err)
	}

	st := ebpf.Stats() // e.g. kubehero_ebpf_flows_emitted_total = st.FlowsEmitted
	_ = st
}
