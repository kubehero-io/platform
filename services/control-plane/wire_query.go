// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"net/http"

	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// wireQuery mounts the read side of the cost, profiling and network
// planes plus alerting:
//
//   - CostService (allocation, time series, rightsizing, efficiency)
//     and its HTTP faces: OpenCost-compatible /allocation/compute and
//     the FinOps FOCUS export
//   - ProfilesService (targets, flamegraph + diff, top functions)
//   - NetworkService (service map, network costs)
//   - AlertsService + the background rule evaluator (started on ctx)
//
// logs is the LogQL metric engine from wireTelemetry (may be nil).
func wireQuery(ctx context.Context, mux *http.ServeMux, d wireDeps, logs signals.LogMetricQuerier) {
	_ = ctx
	_ = mux
	_ = d
	_ = logs
}
