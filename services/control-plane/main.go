// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"connectrpc.com/connect"

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
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
	root.AddCommand(serveCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the control plane HTTP server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	return c
}

func serve(parent context.Context, addr string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// ─── storage ──────────────────────────────────────────────────────────
	// Both are optional on startup — the control-plane degrades gracefully
	// to read-only-stub mode when env is missing (useful for docs builds,
	// integration testing, and the kind demo profile that runs without
	// persistent stores).
	pg, err := db.Open(parent, log, db.Options{URL: os.Getenv("DATABASE_URL")})
	if err != nil {
		log.Warn("postgres unavailable, running in stub mode", "err", err)
	} else {
		defer pg.Close()
	}

	ch, err := clickhouse.Open(parent, log, clickhouse.Options{DSN: os.Getenv("CLICKHOUSE_URL")})
	if err != nil {
		log.Warn("clickhouse unavailable, running in stub mode", "err", err)
	} else {
		defer ch.Close()
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
	}
	authCfg := auth.Config{
		APIKeys:        auth.ParseAPIKeys(os.Getenv("KUBEHERO_API_KEYS")),
		OIDCIssuer:     os.Getenv("OIDC_ISSUER_URL"),
		OIDCAudience:   os.Getenv("OIDC_AUDIENCE"),
		GroupRoles:     auth.ParseGroupRoles(os.Getenv("KUBEHERO_GROUP_ROLES")),
		AllowAnonymous: os.Getenv("KUBEHERO_REQUIRE_AUTH") != "true",
		Logger:         log,
	}
	if authCfg.OIDCIssuer != "" {
		// Lazy-fetched JWKS cache — first verified token triggers the
		// initial /.well-known/openid-configuration + jwks_uri pull.
		// 1h TTL with auto-refresh on kid miss handles key rotation.
		authCfg.JWKS = auth.NewJWKSCache(authCfg.OIDCIssuer)
	}
	interceptors := connect.WithInterceptors(auth.NewInterceptor(authCfg))
	path, handler := kuberov1connect.NewControlPlaneServiceHandler(rpc.New(rpcOpts), interceptors)
	mux.Handle(path, handler)

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

	ctx, cancel := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
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
