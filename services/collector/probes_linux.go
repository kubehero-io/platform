// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build linux

package main

import (
	"context"
	"log/slog"
)

// startProbes attaches eBPF probes for scheduler and memory pressure events.
// Stubbed for scaffold — real implementation uses github.com/cilium/ebpf.
func startProbes(_ context.Context, log *slog.Logger) {
	log.Info("ebpf probes: scaffold stub (linux)")
}
