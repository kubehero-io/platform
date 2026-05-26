// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

//go:build !linux

package main

import (
	"context"
	"log/slog"
)

func startProbes(_ context.Context, log *slog.Logger) {
	log.Info("ebpf probes: unavailable (non-linux)")
}
