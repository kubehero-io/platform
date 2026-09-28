// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/alerter"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
	"github.com/kubehero-io/platform/services/control-plane/internal/db"
	"github.com/kubehero-io/platform/services/control-plane/internal/rpc"
	"github.com/kubehero-io/platform/services/control-plane/internal/scim"
	"github.com/kubehero-io/platform/services/control-plane/internal/store"
)

func main() {
	root := &cobra.Command{
		Use:   "control-plane",
		Short: "KubeHero control plane — policy engine, audit log, RPC surface",
	}
	root.AddCommand(serveCmd(), migrateCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane HTTP server",
		Long: `Run the control plane HTTP server.

Configuration is via environment variables:

  DATABASE_URL                  PostgreSQL DSN (clusters, policies, audit log).
  CLICKHOUSE_URL                ClickHouse DSN (pod_cost_1s time series: burn
                                rate, ingest, spend-anomaly detection).
  KUBEHERO_DEMO_MODE            Unset/default: RPCs without a backing store fall
                                back to built-in demo fixtures (DEMO mode). Set
                                to "false" (or "0") to hard-disable fixtures —
                                those RPCs then return FailedPrecondition
                                instead of fake data. Required for production.
  KUBEHERO_ANOMALY_Z_THRESHOLD  |z| at which ListAnomalies flags a workload's
                                last-hour spend vs its trailing baseline
                                (default 3.0).
  AUDIT_HMAC_KEY                Symmetric secret to sign audit rows.
  KUBEHERO_ALERT_CHANNELS       Comma-separated alert channel URLs
                                (slack://…, pagerduty://…, opsgenie://…)
                                paged when a policy kill-switch is armed
                                or disarmed via ArmPolicy.
  KUBEHERO_API_KEYS             Static API keys (see auth.ParseAPIKeys).
  KUBEHERO_REQUIRE_AUTH         "true" to reject anonymous callers.
  OIDC_ISSUER_URL / OIDC_AUDIENCE / KUBEHERO_GROUP_ROLES
                                OIDC verification + group→role mapping.
  KUBEHERO_SCIM_TOKEN           Enables SCIM 2.0 user provisioning.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	return c
}

// demoFixturesDisabled reports whether KUBEHERO_DEMO_MODE hard-disables
// the demo-fixture fallback. Only an explicit "false"/"0" disables;
// unset keeps the graceful stub-mode default so docs builds, helm
// template previews, and the kind demo profile keep working.
func demoFixturesDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KUBEHERO_DEMO_MODE"))) {
	case "false", "0":
		return true
	}
	return false
}

// anomalyZThreshold reads KUBEHERO_ANOMALY_Z_THRESHOLD; 0 lets the
// provider fall back to its default (3.0).
func anomalyZThreshold(log *slog.Logger) float64 {
	raw := strings.TrimSpace(os.Getenv("KUBEHERO_ANOMALY_Z_THRESHOLD"))
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		log.Warn("invalid KUBEHERO_ANOMALY_Z_THRESHOLD, using default 3.0", "value", raw)
		return 0
	}
	return v
}

func serve(parent context.Context, addr string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	// Packages that log without an injected logger (the store-backed
	// ControlPlane RPCs, library code) emit the same JSON lines.
	slog.SetDefault(log)
	var err error

	// ─── storage ──────────────────────────────────────────────────────────
	// Both are optional on startup — the control-plane degrades gracefully
	// to read-only-stub mode when env is missing (useful for docs builds,
	// integration testing, and the kind demo profile that runs without
	// persistent stores).
	// A store whose URL is set must come up: on a fresh install the
	// database pod is often still starting, so retry with backoff for
	// KUBEHERO_STORE_WAIT (default 2m) and then exit non-zero so the
	// orchestrator restarts us — never silently fall back to stub mode
	// with a configured store.
	storeWait := durationEnv(log, "KUBEHERO_STORE_WAIT", 2*time.Minute)
	var pg, ch *sql.DB
	if url := os.Getenv("DATABASE_URL"); url != "" {
		pg, err = openWithRetry(parent, log, "postgres", storeWait, func(ctx context.Context) (*sql.DB, error) {
			return db.Open(ctx, log, db.Options{URL: url})
		})
		if err != nil {
			return err
		}
		defer pg.Close()
	} else {
		log.Warn("DATABASE_URL unset — no Postgres (clusters, policies, audit and alert rules are not persisted)")
	}
	if dsn := os.Getenv("CLICKHOUSE_URL"); dsn != "" {
		ch, err = openWithRetry(parent, log, "clickhouse", storeWait, func(ctx context.Context) (*sql.DB, error) {
			return clickhouse.Open(ctx, log, clickhouse.Options{DSN: dsn})
		})
		if err != nil {
			return err
		}
		defer ch.Close()
	} else {
		log.Warn("CLICKHOUSE_URL unset — no time-series store (cost, logs, profiles, flows are not stored)")
	}

	fixturesOff := demoFixturesDisabled()
	if pg == nil && ch == nil {
		if fixturesOff {
			log.Warn("no Postgres or ClickHouse configured and KUBEHERO_DEMO_MODE=false — data RPCs will return FailedPrecondition until DATABASE_URL/CLICKHOUSE_URL are set")
		} else {
			log.Warn("DEMO MODE: no Postgres or ClickHouse configured — serving built-in demo fixtures, NOT real data (set DATABASE_URL/CLICKHOUSE_URL, or KUBEHERO_DEMO_MODE=false to fail loudly instead)")
		}
	}

	// ─── HTTP + RPC ───────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		// readyz reflects actual storage health
		if pg != nil {
			if err := pg.PingContext(parent); err != nil {
				http.Error(w, "postgres not ready", http.StatusServiceUnavailable)
				return
			}
		}
		if ch != nil {
			if err := ch.PingContext(parent); err != nil {
				http.Error(w, "clickhouse not ready", http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# HELP kubehero_up 1 if process is running")
		_, _ = fmt.Fprintln(w, "# TYPE kubehero_up gauge")
		_, _ = fmt.Fprintln(w, `kubehero_up{service="control-plane"} 1`)
	})

	var rpcOpts rpc.Options
	if pg != nil {
		rpcOpts.Clusters = &store.ClustersPG{DB: pg}
		rpcOpts.Policies = &store.PoliciesPG{DB: pg}
		// AUDIT_HMAC_KEY is the symmetric secret used to sign audit rows
		// so a downstream SIEM can verify exports. Empty = signing disabled
		// (dev mode); the entry still persists, signature column stays "".
		secret := []byte(os.Getenv("AUDIT_HMAC_KEY"))
		rpcOpts.Audit = &store.AuditPG{DB: pg, Secret: secret}
		if len(secret) == 0 {
			log.Warn("AUDIT_HMAC_KEY unset, audit rows will be unsigned (dev only)")
		}
	}
	if ch != nil {
		rpcOpts.BurnRate = &clickhouse.BurnRateProvider{DB: ch}
		rpcOpts.PodCost = &clickhouse.PodCostWriter{DB: ch}
		rpcOpts.Anomalies = &clickhouse.SpendAnomalyProvider{DB: ch, ZThreshold: anomalyZThreshold(log)}
	}
	rpcOpts.DemoFixturesDisabled = fixturesOff
	// KUBEHERO_ALERT_CHANNELS wires the ArmPolicy page-out. Same channel
	// grammar as policy escalation steps: scheme selects the provider.
	var alertChannels []string
	for _, ch := range strings.Split(os.Getenv("KUBEHERO_ALERT_CHANNELS"), ",") {
		if ch = strings.TrimSpace(ch); ch != "" {
			alertChannels = append(alertChannels, ch)
		}
	}
	// One router serves both the ArmPolicy page-out and the alert rule
	// evaluator; each rule carries its own channels.
	router := alerter.NewRouter(alerter.NewSlack(), alerter.NewPagerDuty(), alerter.NewOpsGenie())
	if len(alertChannels) > 0 {
		rpcOpts.Alerts = router
		rpcOpts.AlertChannels = alertChannels
		log.Info("alerting wired", "channels", len(alertChannels))
	}
	rpcOpts.PG = pg
	rpcOpts.CH = ch
	authCfg := auth.Config{
		APIKeys:        auth.ParseAPIKeys(os.Getenv("KUBEHERO_API_KEYS")),
		OIDCIssuer:     os.Getenv("OIDC_ISSUER_URL"),
		OIDCAudience:   os.Getenv("OIDC_AUDIENCE"),
		GroupRoles:     auth.ParseGroupRoles(os.Getenv("KUBEHERO_GROUP_ROLES")),
		AllowAnonymous: os.Getenv("KUBEHERO_REQUIRE_AUTH") != "true",
		Logger:         log,
	}
	if pg != nil {
		// Enrollment tokens minted by RegisterCluster authenticate that
		// cluster's collector + operator (hash compared, never stored).
		authCfg.ClusterTokens = &store.ClustersPG{DB: pg}
	}
	if authCfg.OIDCIssuer != "" {
		// Lazy-fetched JWKS cache — first verified token triggers the
		// initial /.well-known/openid-configuration + jwks_uri pull.
		// 1h TTL with auto-refresh on kid miss handles key rotation.
		authCfg.JWKS = auth.NewJWKSCache(authCfg.OIDCIssuer)
	}
	rpcOpts.AuthRequired = !authCfg.AllowAnonymous
	interceptors := connect.WithInterceptors(auth.NewInterceptor(authCfg))
	path, handler := kuberov1connect.NewControlPlaneServiceHandler(rpc.New(rpcOpts), interceptors)
	mux.Handle(path, handler)

	// Signal engines: logs / profiles / flows / usage / events ingest +
	// LogQL, then cost allocation, profiling, network and alerting.
	// Background loops (alert evaluation, tail pollers) stop with ctx.
	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	deps := wireDeps{
		Log:                  log,
		PG:                   pg,
		CH:                   ch,
		Handler:              []connect.HandlerOption{interceptors},
		DemoFixturesDisabled: fixturesOff,
		Alerts:               router,
	}
	logEngine := wireTelemetry(ctx, mux, deps)
	wireQuery(ctx, mux, deps, logEngine)

	// SCIM 2.0 — opt-in via KUBEHERO_SCIM_TOKEN. When unset, every
	// non-discovery SCIM endpoint returns 401, so external IdPs can
	// detect "SCIM disabled" via a clear status code rather than a
	// 404. Discovery endpoints stay public per RFC 7644 §4.
	scimToken := os.Getenv("KUBEHERO_SCIM_TOKEN")
	scimStore := scim.NewMemoryStore() // swap for SCIMPG once orgs+users tables ship
	mux.Handle("/scim/v2/", scim.AuthMiddleware(scimToken, scim.Handler(scimStore)))
	if scimToken == "" {
		log.Info("SCIM disabled — set KUBEHERO_SCIM_TOKEN to enable IdP user provisioning")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           h2c.NewHandler(mux, &http2.Server{}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-errCh:
		return err
	}

	sh, cancelSh := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSh()
	return srv.Shutdown(sh)
}

// migrateCmd applies Postgres and ClickHouse schema migrations and
// exits — what the Helm pre-install/pre-upgrade hook runs. Each store
// is migrated only when its URL is set; a configured store that can't
// be reached is an error (the hook retries).
func migrateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "migrate",
		Short: "Apply Postgres + ClickHouse schema migrations, then exit",
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
			ctx := cmd.Context()
			ran := 0
			if os.Getenv("DATABASE_URL") != "" {
				pg, err := db.Open(ctx, log, db.Options{URL: os.Getenv("DATABASE_URL")})
				if err != nil {
					return fmt.Errorf("postgres: %w", err)
				}
				_ = pg.Close()
				ran++
			}
			if os.Getenv("CLICKHOUSE_URL") != "" {
				ch, err := clickhouse.Open(ctx, log, clickhouse.Options{DSN: os.Getenv("CLICKHOUSE_URL")})
				if err != nil {
					return fmt.Errorf("clickhouse: %w", err)
				}
				_ = ch.Close()
				ran++
			}
			if ran == 0 {
				log.Warn("migrate: neither DATABASE_URL nor CLICKHOUSE_URL is set — nothing to do")
			}
			return nil
		},
	}
}

// openWithRetry calls open until it succeeds, ctx ends, or maxWait
// elapses, backing off from 1s to 10s between attempts.
func openWithRetry(ctx context.Context, log *slog.Logger, name string, maxWait time.Duration, open func(context.Context) (*sql.DB, error)) (*sql.DB, error) {
	deadline := time.Now().Add(maxWait)
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		conn, err := open(ctx)
		if err == nil {
			return conn, nil
		}
		if time.Now().Add(backoff).After(deadline) {
			return nil, fmt.Errorf("%s unavailable after %s (%d attempts): %w", name, maxWait, attempt, err)
		}
		log.Warn("store not ready, retrying", "store", name, "attempt", attempt, "in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Second)
	}
}

// durationEnv parses a Go duration from env, falling back to def.
func durationEnv(log *slog.Logger, key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Warn("invalid duration, using default", "env", key, "value", raw, "default", def.String())
		return def
	}
	return d
}
