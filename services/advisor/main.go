// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Command advisor is KubeHero's agentic layer: it monitors the fleet's
// cost/ops telemetry, explains what changed, answers operators'
// questions with a read-only investigation loop, and PROPOSES guarded
// actions.
//
// GUARDRAILS (non-negotiable):
//   - The advisor is READ-ONLY. It never calls the Kubernetes API and
//     never calls control-plane mutation RPCs — its only inputs are the
//     control-plane's read RPCs (internal/backend exposes nothing else).
//   - Every proposed action's crd_yaml is a manifest the human (or CLI)
//     applies through the operator's existing arming flow. Nothing the
//     advisor emits executes automatically.
//   - LLM output is validated: action kinds are whitelisted, impacts
//     must be finite and >= 0, and crd_yaml must parse to a BudgetPolicy
//     / CeilingPolicy / RightsizingPolicy — otherwise the action is
//     downgraded to an investigate-only proposal. Evidence links must be
//     dashboard-relative.
//   - The endpoints never fail because Anthropic is down or declines: the
//     LLM tier falls back to the deterministic rules tier on any error.
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

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/brain/llm"
	"github.com/kubehero-io/platform/services/advisor/internal/brain/rules"
	"github.com/kubehero-io/platform/services/advisor/internal/investigate"
	"github.com/kubehero-io/platform/services/advisor/internal/rpc"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

func main() {
	root := &cobra.Command{
		Use:   "advisor",
		Short: "KubeHero advisor — agentic briefings + proposed guarded actions",
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
		Short: "Run the advisor HTTP server",
		Long: `Run the advisor HTTP server.

Configuration is via environment variables:

  CONTROL_PLANE_URL       Base URL of the control-plane's Connect API
                          (e.g. http://control-plane:8080). Unset: the
                          advisor runs over a built-in demo fixture and
                          every answer reports source="demo".
  KUBEHERO_API_TOKEN      Bearer token for the control plane (required
  CONTROL_PLANE_TOKEN     when it runs with KUBEHERO_REQUIRE_AUTH); the
                          first one set wins.
  ANTHROPIC_API_KEY       Enables the LLM tier (Claude). Unset: the
                          deterministic rules tier runs alone. When set,
                          any LLM failure or refusal still falls back to
                          rules — the RPCs never fail because Anthropic is
                          down.
  KUBEHERO_ADVISOR_MODEL  Claude model id (default claude-opus-5).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", ":8083", "listen address")
	return c
}

func serve(parent context.Context, addr string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// ─── data in (tier 1) ─────────────────────────────────────────────────
	var be backend.Backend
	demo := false
	if cpURL := os.Getenv("CONTROL_PLANE_URL"); cpURL != "" {
		token := firstNonEmpty(os.Getenv("KUBEHERO_API_TOKEN"), os.Getenv("CONTROL_PLANE_TOKEN"))
		log.Info("using control-plane source", "url", cpURL, "auth", token != "")
		be = backend.NewConnect(cpURL, token)
	} else {
		log.Warn("DEMO MODE: CONTROL_PLANE_URL unset — briefings and investigations use built-in fixture data (source=\"demo\")")
		be = backend.Demo{}
		demo = true
	}
	src := source.NewControlPlane(be)

	// ─── brains (tier 2, chosen at startup) ───────────────────────────────
	var b brain.Brain = rules.New()
	var inv investigate.Investigator = &investigate.Rules{Backend: be}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		cfg := llm.ConfigFromEnv()
		log.Info("LLM tier enabled (falls back to rules on any failure)", "model", firstNonEmpty(cfg.Model, llm.DefaultModel))
		b = llm.New(b, log, cfg)
		inv = llm.NewInvestigator(be, inv, log, cfg)
	} else {
		log.Info("rules tier active (set ANTHROPIC_API_KEY to enable the LLM tier)")
	}

	// ─── HTTP + RPC ───────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = fmt.Fprintln(w, "# HELP kubehero_up 1 if process is running")
		_, _ = fmt.Fprintln(w, "# TYPE kubehero_up gauge")
		_, _ = fmt.Fprintln(w, `kubehero_up{service="advisor"} 1`)
	})

	adv := rpc.New(b, src, log)
	adv.Investigator = inv
	adv.DemoData = demo
	path, handler := kuberov1connect.NewAdvisorServiceHandler(adv)
	mux.Handle(path, handler)

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

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
