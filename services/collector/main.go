// SPDX-License-Identifier: Apache-2.0
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

	"github.com/kubehero-io/platform/services/collector/internal/ingest"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
)

func main() {
	root := &cobra.Command{
		Use:   "collector",
		Short: "KubeHero node agent — eBPF + cAdvisor + DCGM telemetry",
	}
	root.AddCommand(serveCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func serveCmd() *cobra.Command {
	var addr string
	var demo bool
	c := &cobra.Command{
		Use:   "serve",
		Short: "Run the collector as a DaemonSet",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr, demo)
		},
	}
	c.Flags().StringVar(&addr, "addr", ":8081", "listen address")
	c.Flags().BoolVar(&demo, "demo", true, "emit demo chargeback series until eBPF is wired")
	return c
}

func serve(parent context.Context, addr string, demo bool) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		metrics.WriteSchema(w)
		metrics.WriteSeries(w, metrics.Series{
			Name:   "kubehero_up",
			Labels: map[string]string{"service": "collector"},
			Value:  1,
		})
		if demo {
			for _, s := range metrics.Demo() {
				metrics.WriteSeries(w, s)
			}
		}
	})

	// eBPF programs are loaded in probes_linux.go (build tag stub for now).
	startProbes(parent, log)

	// Cluster-aware synthetic ingest. Reads pods + nodes from the
	// kube API every 5s, attributes synthetic cost from requests +
	// node SKU, ships batches to the cp via Connect-RPC.
	go func() {
		err := ingest.Run(parent, ingest.Config{
			ControlPlaneURL: os.Getenv("CONTROL_PLANE_URL"),
			Token:           os.Getenv("CONTROL_PLANE_TOKEN"),
			ClusterID:       os.Getenv("CLUSTER_ID"),
			Interval:        5 * time.Second,
			Logger:          log,
		})
		if err != nil {
			log.Error("ingest stopped with error", "err", err)
		}
	}()

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
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
	sh, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	return srv.Shutdown(sh)
}
