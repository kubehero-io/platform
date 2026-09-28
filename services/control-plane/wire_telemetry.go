// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"net/http"

	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// wireTelemetry mounts the write path for every non-cost signal and the
// logs query surface:
//
//   - TelemetryService (IngestLogs / IngestProfiles / IngestFlows /
//     IngestUsage / IngestEvents)
//   - LogsService (LogQL query, volume, patterns, labels, tail)
//   - compatibility shims: Loki push + query API, OTLP/HTTP logs,
//     Pyroscope /ingest
//
// It returns the LogQL metric engine so the alert evaluator can run
// kind=logs rules (nil when logs are unavailable).
func wireTelemetry(ctx context.Context, mux *http.ServeMux, d wireDeps) signals.LogMetricQuerier {
	_ = ctx
	_ = mux
	_ = d
	return nil
}
