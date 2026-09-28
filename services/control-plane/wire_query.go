// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerts"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/compat/opencost"
	"github.com/kubehero-io/platform/services/control-plane/internal/cost"
	"github.com/kubehero-io/platform/services/control-plane/internal/focus"
	"github.com/kubehero-io/platform/services/control-plane/internal/httpauth"
	"github.com/kubehero-io/platform/services/control-plane/internal/insights"
	"github.com/kubehero-io/platform/services/control-plane/internal/network"
	"github.com/kubehero-io/platform/services/control-plane/internal/profiles"
	"github.com/kubehero-io/platform/services/control-plane/internal/rightsizing"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
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
//
// Every service degrades like the ControlPlane RPCs: without ClickHouse
// it serves labelled demo data, or FailedPrecondition when
// KUBEHERO_DEMO_MODE=false. Alert rules, state and silences live in
// Postgres when configured, in memory otherwise.
//
// Environment read here:
//
//	KUBEHERO_DASHBOARD_URL  base URL for deep links in alert
//	                        notifications (e.g. https://kubehero.example.com)
func wireQuery(ctx context.Context, mux *http.ServeMux, d wireDeps, logs signals.LogMetricQuerier) {
	log := d.Log
	if log == nil {
		log = slog.Default()
	} else {
		// Query packages that log without an injected logger (the store-
		// backed ControlPlane RPCs) must emit the same JSON.
		slog.SetDefault(log)
	}
	names := clusters.NewResolver(d.PG, log)

	var (
		costEngine *cost.Engine
		rs         *rightsizing.Engine
		ins        *insights.Engine
	)
	if d.CH != nil {
		costEngine = &cost.Engine{CH: d.CH, Clusters: names, Log: log}
		rs = &rightsizing.Engine{CH: d.CH, Clusters: names}
		ins = &insights.Engine{CH: d.CH, Clusters: names}
	}

	costSvc := &cost.Service{Engine: costEngine, Rightsizing: rs, Clusters: names,
		DemoDisabled: d.DemoFixturesDisabled, Log: log}
	mux.Handle(kuberov1connect.NewCostServiceHandler(costSvc, d.Handler...))
	mux.Handle(kuberov1connect.NewProfilesServiceHandler(&profiles.Service{CH: d.CH, Clusters: names,
		DemoDisabled: d.DemoFixturesDisabled}, d.Handler...))
	mux.Handle(kuberov1connect.NewNetworkServiceHandler(&network.Service{CH: d.CH, Clusters: names,
		DemoDisabled: d.DemoFixturesDisabled}, d.Handler...))

	// ─── alerting ────────────────────────────────────────────────────
	router := d.Alerts
	if router == nil {
		router = alerter.NewRouter()
	}
	var (
		alertStore alerts.Store = alerts.NewMemoryStore()
		leader     alerts.Leader
		policies   store.PolicyLister
	)
	if d.PG != nil {
		alertStore = &alerts.PGStore{DB: d.PG}
		leader = &alerts.PGLeader{DB: d.PG}
		policies = &store.PoliciesPG{DB: d.PG}
	}
	src := &alerts.StoreSources{CH: d.CH, Logs: logs, Policies: policies, Insights: ins, Clusters: names}
	if d.CH != nil {
		src.BurnRate = &clickhouse.BurnRateProvider{DB: d.CH}
		src.Spend = &clickhouse.SpendAnomalyProvider{DB: d.CH, ZThreshold: anomalyZThreshold(log)}
	}
	engine := &alerts.Engine{Store: alertStore, Sources: src, Notifier: router, Leader: leader, Log: log,
		DashboardURL: dashboardURL(log)}
	mux.Handle(kuberov1connect.NewAlertsServiceHandler(&alerts.Service{Store: alertStore, Sources: src, Engine: engine,
		Channels: router, Demo: d.CH == nil && !d.DemoFixturesDisabled}, d.Handler...))
	go engine.Run(ctx)

	// ─── HTTP faces (same authentication as the Connect handlers) ────
	viewer := httpauth.Middleware(auth.RoleViewer, d.Handler...)
	oc := viewer(&opencost.Handler{Cost: costSvc, Clusters: names, Log: log})
	for _, p := range opencost.Paths {
		mux.Handle("GET "+p, oc)
	}
	mux.Handle("GET "+focus.Path, viewer(&focus.Handler{Cost: costSvc, CH: d.CH, Clusters: names, Log: log}))

	log.Info("query plane wired",
		"clickhouse", d.CH != nil, "postgres", d.PG != nil, "logs_engine", logs != nil,
		"alert_store", map[bool]string{true: "postgres", false: "memory"}[alertStore.Persistent()],
		"opencost", strings.Join(opencost.Paths, ","), "focus", focus.Path)
}

// dashboardURL reads KUBEHERO_DASHBOARD_URL; anything but an absolute
// http(s) URL is ignored with a warning (notifications then carry no
// deep link rather than a broken one).
func dashboardURL(log *slog.Logger) string {
	raw := strings.TrimSpace(os.Getenv("KUBEHERO_DASHBOARD_URL"))
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		log.Warn("ignoring KUBEHERO_DASHBOARD_URL: want an absolute http(s) URL", "value", raw)
		return ""
	}
	return strings.TrimSuffix(u.String(), "/")
}
