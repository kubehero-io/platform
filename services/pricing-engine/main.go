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

	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/pricing-engine/internal/pricing"
	"github.com/kubehero-io/platform/services/pricing-engine/internal/rpc"
)

func main() {
	root := &cobra.Command{
		Use:   "pricing-engine",
		Short: "KubeHero pricing engine — multi-cloud instance price normalization",
	}
	root.AddCommand(quoteCmd(), serveCmd())
	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func quoteCmd() *cobra.Command {
	var cloud, sku, region, lifecycle string
	c := &cobra.Command{
		Use:   "quote",
		Short: "Quote a per-hour price for an instance SKU",
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg := pricing.NewDefaultRegistry()
			// Wire the live source for the cloud being queried. The
			// registry falls back to the static map when the live API
			// returns ErrUnimplemented (e.g. spot via the AWS public
			// price list) or ErrNotFound, so the CLI always returns a
			// number for SKUs we know.
			switch pricing.Cloud(cloud) {
			case pricing.CloudAWS:
				reg.WithLive(pricing.NewAWS())
			case pricing.CloudGCP:
				reg.WithLive(pricing.NewGCP())
			case pricing.CloudAzure:
				reg.WithLive(pricing.NewAzure())
			}
			q, err := reg.Quote(cmd.Context(), pricing.Cloud(cloud), sku, region, lifecycle)
			if err != nil {
				return err
			}
			fmt.Printf("%s %s %s %s $%.4f/hr %s\n",
				q.Cloud, q.SKU, q.Region, q.Lifecycle, q.PricePerHour, q.Currency)
			return nil
		},
	}
	c.Flags().StringVar(&cloud, "cloud", "aws", "aws | gcp | azure")
	c.Flags().StringVar(&sku, "sku", "m5.large", "instance SKU")
	c.Flags().StringVar(&region, "region", "us-east-1", "region")
	c.Flags().StringVar(&lifecycle, "lifecycle", "on-demand", "on-demand | spot | savings-plan | committed")
	return c
}

func serveCmd() *cobra.Command {
	var addr string
	c := &cobra.Command{
		Use:   "serve",
		Short: "Serve the PricingService over Connect RPC",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return serve(cmd.Context(), addr)
		},
	}
	c.Flags().StringVar(&addr, "addr", ":8082", "listen address")
	return c
}

func serve(parent context.Context, addr string) error {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "ok")
	})
	path, handler := kuberov1connect.NewPricingServiceHandler(rpc.New())
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
