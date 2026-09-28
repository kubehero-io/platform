// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/batcher"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/loki"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/otlp"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/pyroscope"
	"github.com/kubehero-io/platform/services/control-plane/internal/logs"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
	"github.com/kubehero-io/platform/services/control-plane/internal/telemetry"
)

// telemetryReadMaxBytes bounds one decompressed ingest request (10k
// log lines of typical size fit comfortably; 64 KiB lines are cut at
// ingest anyway).
const telemetryReadMaxBytes = 64 << 20

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
// kind=logs rules (nil when logs are unavailable — including demo mode:
// alerts must never fire on demo data).
//
// Environment (beyond main's CLICKHOUSE_URL / auth / demo settings):
//
//	KUBEHERO_LOG_USD_PER_GB             $/GB for the log-volume cost estimate (0.50)
//	KUBEHERO_NET_EGRESS_USD_PER_GB      internet egress $/GB for every cloud
//	                                    (default aws 0.09, gcp 0.12, azure 0.087)
//	KUBEHERO_NET_CROSS_ZONE_USD_PER_GB  cross-zone $/GB (default aws/gcp 0.01, azure 0)
//	KUBEHERO_INGEST_LOG_BUFFER_MB       accepted-but-unwritten log buffer (256)
//	KUBEHERO_COMPAT_DEFAULT_CLUSTER     cluster for Loki/OTLP/Pyroscope pushes that
//	                                    name none (no cluster label, no
//	                                    X-Scope-OrgID, no enrollment token) ("default")
func wireTelemetry(ctx context.Context, mux *http.ServeMux, d wireDeps) signals.LogMetricQuerier {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "telemetry")
	// Plain-HTTP endpoints and server streams authenticate through the
	// exact interceptor chain the Connect services use.
	authn := httpauth.New(d.Handler...)
	defaultCluster := strings.TrimSpace(os.Getenv("KUBEHERO_COMPAT_DEFAULT_CLUSTER"))
	if defaultCluster == "" {
		defaultCluster = "default"
	}

	// The heavy signal tables use the native protocol (column-oriented
	// batch inserts, streamed reads) on the same DSN main migrated.
	var native driver.Conn
	var signalWriter *clickhouse.SignalWriter
	var store logs.Store
	if d.CH != nil {
		conn, err := clickhouse.OpenNative(ctx, os.Getenv("CLICKHOUSE_URL"))
		if err != nil {
			log.Error("clickhouse native connection failed: logs, profiles, flows, usage and events will NOT be stored", "err", err)
		} else {
			native = conn
			signalWriter = &clickhouse.SignalWriter{Conn: native}
			store = &logs.CHStore{Conn: native}
		}
	}

	// ─── write path ──────────────────────────────────────────────────────
	pricing := telemetry.PricingFromEnv(os.Getenv, log)
	topts := telemetry.Options{
		Pricing:       &pricing,
		LogQueueBytes: telemetry.LogQueueBytesFromEnv(os.Getenv, log),
		Log:           log,
	}
	if signalWriter != nil {
		topts.Writer = signalWriter
		topts.Clouds = &telemetry.CloudResolver{Lookup: telemetry.ClickHouseCloudLookup(d.CH)}
	}
	tel := telemetry.New(topts)
	tel.Run(ctx)
	if native != nil {
		// On shutdown the writers drain what was accepted, then the
		// connection closes.
		go func() {
			<-ctx.Done()
			<-tel.Done()
			_ = native.Close()
		}()
	}
	if !tel.Enabled() {
		log.Warn("TelemetryService is accepting and dropping every row: ClickHouse is not configured")
	}
	go logIngestStats(ctx, tel, log)

	path, h := kuberov1connect.NewTelemetryServiceHandler(tel, handlerOpts(d.Handler, connect.WithReadMaxBytes(telemetryReadMaxBytes))...)
	mux.Handle(path, h)

	// ─── logs engine ─────────────────────────────────────────────────────
	engine := logs.New(logs.Options{
		Store:                store,
		DemoFixturesDisabled: d.DemoFixturesDisabled,
		USDPerGB:             floatEnv(log, "KUBEHERO_LOG_USD_PER_GB", logs.DefaultUSDPerGB),
		Log:                  log,
	})
	switch {
	case engine.Demo():
		log.Warn("DEMO MODE: LogsService serves generated demo logs labelled source=\"demo\" (set CLICKHOUSE_URL for real logs)")
	case engine.Source() == "":
		log.Warn("LogsService unavailable: no ClickHouse and KUBEHERO_DEMO_MODE=false")
	}
	path, h = kuberov1connect.NewLogsServiceHandler(&logs.Service{Engine: engine},
		handlerOpts(d.Handler, connect.WithInterceptors(authn.StreamInterceptor()))...)
	mux.Handle(path, h)

	// ─── compatibility shims ─────────────────────────────────────────────
	mux.Handle("/loki/api/v1/push", &loki.PushHandler{Writer: tel, Auth: authn, DefaultCluster: defaultCluster, Log: log})
	(&loki.QueryAPI{Engine: engine, Auth: authn, Log: log}).Mount(mux)
	mux.Handle("/v1/logs", &otlp.Handler{Writer: tel, Auth: authn, DefaultCluster: defaultCluster, Log: log})
	mux.Handle("/ingest", &pyroscope.Handler{Writer: tel, Auth: authn, DefaultCluster: defaultCluster, Log: log})

	if engine.Source() != "clickhouse" {
		return nil
	}
	return engine
}

// handlerOpts copies the shared options before adding more, so the
// slice other services receive is never aliased.
func handlerOpts(base []connect.HandlerOption, extra ...connect.HandlerOption) []connect.HandlerOption {
	out := make([]connect.HandlerOption, 0, len(base)+len(extra))
	return append(append(out, base...), extra...)
}

func floatEnv(log *slog.Logger, key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v < 0 {
		log.Warn("invalid number, using default", "env", key, "value", raw, "default", def)
		return def
	}
	return v
}

// logIngestStats reports ingest totals once a minute while rows are
// flowing (failures and refusals are also logged as they happen).
func logIngestStats(ctx context.Context, tel *telemetry.Service, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	last := map[string]batcher.Stats{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for _, s := range tel.Stats() {
			prev := last[s.Name]
			if s.Written == prev.Written && s.Failed == prev.Failed && s.Rejected == prev.Rejected {
				continue
			}
			log.Info("ingest", "table", s.Name,
				"written", s.Written-prev.Written, "failed", s.Failed-prev.Failed, "rejected", s.Rejected-prev.Rejected,
				"queued_rows", s.QueuedRows, "queued_bytes", s.QueuedBytes, "last_error", s.LastError)
			last[s.Name] = s
		}
	}
}
