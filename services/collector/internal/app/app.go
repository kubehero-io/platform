// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package app wires the collector's pipelines into one process and owns
// its lifecycle: start order (informers before consumers), degraded
// modes (no control plane, no kubelet stats, no eBPF, no leader), and a
// shutdown that drains the ship queues before exiting and only then
// checkpoints log offsets, so a rolling restart neither loses nor
// duplicates what was already read.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
	"k8s.io/client-go/kubernetes"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/collector/internal/ingest"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
	"github.com/kubehero-io/platform/services/collector/internal/ship"
)

// Config is everything the process needs, assembled by main from flags
// and environment.
type Config struct {
	Version string
	Addr    string
	Demo    bool
	Logger  *slog.Logger

	Kubeconfig   string
	NodeName     string
	PodName      string
	PodNamespace string
	ClusterID    string

	ControlPlaneURL   string
	ControlPlaneToken string
	PricingEngineURL  string

	ScanInterval       time.Duration
	KubeletURL         string
	KubeletInsecureTLS bool
	KubeletStatsTTL    time.Duration

	// ShutdownTimeout bounds the final queue flush.
	ShutdownTimeout time.Duration

	// Client overrides the Kubernetes client (tests).
	Client kubernetes.Interface
}

// App is a running collector.
type App struct {
	cfg   Config
	log   *slog.Logger
	kube  kubernetes.Interface
	local *kube.LocalCache

	ship    *ship.Client
	scanner *ingest.Scanner
	ready   atomic.Bool

	flushers []func(context.Context)
}

// Run starts every pipeline and blocks until ctx is cancelled, then
// shuts down gracefully.
func Run(ctx context.Context, cfg Config) error {
	a, err := newApp(cfg)
	if err != nil {
		return err
	}
	return a.run(ctx)
}

func newApp(cfg Config) (*App, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ScanInterval <= 0 {
		cfg.ScanInterval = 5 * time.Second
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = 10 * time.Second
	}
	a := &App{cfg: cfg, log: cfg.Logger}
	if cfg.ClusterID == "" {
		a.log.Warn("CLUSTER_ID unset — the control plane will attribute rows to the token's cluster, or drop them")
	}
	if cfg.NodeName == "" {
		a.log.Warn("NODE_NAME unset — running in ALL-NODES mode: this process prices every node in the cluster. " +
			"Fine for local development against a kubeconfig; as a DaemonSet it multiplies reported cost by the node count. " +
			"Set NODE_NAME from the downward API (spec.nodeName).")
	}

	a.kube = cfg.Client
	if a.kube == nil {
		c, err := kube.NewClient(cfg.Kubeconfig, a.userAgent())
		if err != nil {
			return nil, fmt.Errorf("kubernetes client: %w", err)
		}
		a.kube = c
	}
	local, err := kube.NewLocalCache(a.kube, cfg.NodeName, a.log)
	if err != nil {
		return nil, err
	}
	a.local = local

	if cfg.ControlPlaneURL != "" {
		c, err := ship.NewClient(ship.Options{URL: cfg.ControlPlaneURL, Token: cfg.ControlPlaneToken, UserAgent: a.userAgent()})
		if err != nil {
			return nil, err
		}
		a.ship = c
	} else {
		a.log.Warn("CONTROL_PLANE_URL unset — nothing is shipped; cost is computed and exposed on /metrics only, log tailing and profile scraping are disabled")
	}
	return a, nil
}

func (a *App) userAgent() string { return "kubehero-collector/" + a.cfg.Version }

func (a *App) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv, srvErr := a.serveHTTP()

	a.local.Start(ctx)
	syncCtx, syncCancel := context.WithTimeout(ctx, 2*time.Minute)
	err := a.local.WaitForSync(syncCtx)
	syncCancel()
	if err != nil {
		if ctx.Err() != nil {
			return a.shutdown(srv, nil)
		}
		return fmt.Errorf("waiting for the pod/node cache: %w", err)
	}
	a.ready.Store(true)
	a.log.Info("kubernetes cache synced", "node", a.cfg.NodeName, "pods", len(a.local.Pods()))

	var wg sync.WaitGroup
	owners := kube.NewOwnerResolver(a.kube, a.log)
	stats, err := a.statsProvider()
	if err != nil {
		return err
	}

	// ── cost ──────────────────────────────────────────────────────────
	var emitCost func(*kuberov1.IngestPodCostRequest)
	if a.ship != nil {
		q := ship.NewQueue(ship.QueueConfig{Signal: "cost", MaxItems: 50_000, MaxBytes: 16 << 20, Timeout: 10 * time.Second}, a.ship.SendPodCost, a.log)
		a.startQueue(ctx, &wg, q.Run, q.Flush)
		emitCost = func(r *kuberov1.IngestPodCostRequest) {
			q.Enqueue(ship.Batch[*kuberov1.IngestPodCostRequest]{Req: r, Items: len(r.Samples) + len(r.Nodes), Bytes: proto.Size(r)})
		}
	}
	pricingClient := a.pricingClient()
	a.scanner = ingest.New(ingest.Config{
		ClusterID: a.cfg.ClusterID, NodeName: a.cfg.NodeName, Interval: a.cfg.ScanInterval, Logger: a.log,
	}, a.local, owners, stats, ingest.NewPricer(pricingClient, a.log), emitCost)
	a.goLoop(ctx, &wg, a.scanner.Run)

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		cancel()
		wg.Wait()
		return errors.Join(err, a.shutdown(srv, nil))
	}
	a.log.Info("shutting down")
	wg.Wait()
	return a.shutdown(srv, nil)
}

// shutdown drains the ship queues (bounded by ShutdownTimeout), runs the
// post-flush hooks (log checkpoints), and stops the HTTP server.
func (a *App) shutdown(srv *http.Server, after []func()) error {
	fctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	var fw sync.WaitGroup
	for _, f := range a.flushers {
		fw.Add(1)
		go func() { defer fw.Done(); f(fctx) }()
	}
	fw.Wait()
	for _, f := range after {
		f()
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if srv != nil {
		return srv.Shutdown(sctx)
	}
	return nil
}

func (a *App) startQueue(ctx context.Context, wg *sync.WaitGroup, run func(context.Context), flush func(context.Context)) {
	a.goLoop(ctx, wg, run)
	a.flushers = append(a.flushers, flush)
}

func (a *App) goLoop(ctx context.Context, wg *sync.WaitGroup, f func(context.Context)) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		f(ctx)
	}()
}

func (a *App) statsProvider() (kubeletstats.Provider, error) {
	var inner kubeletstats.Provider
	if a.cfg.KubeletURL != "" {
		d, err := kubeletstats.NewDirect(kubeletstats.DirectConfig{
			URL:       a.cfg.KubeletURL,
			TokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
			CAFile:    "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
			Insecure:  a.cfg.KubeletInsecureTLS,
		})
		if err != nil {
			return nil, fmt.Errorf("kubelet stats: %w", err)
		}
		inner = d
	} else {
		inner = kubeletstats.New(a.kube)
	}
	ttl := a.cfg.KubeletStatsTTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return kubeletstats.NewCached(inner, ttl), nil
}

func (a *App) serveHTTP() (*http.Server, <-chan error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !a.ready.Load() {
			http.Error(w, "informer cache not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", a.serveMetrics)
	srv := &http.Server{Addr: a.cfg.Addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("listening", "addr", a.cfg.Addr, "version", a.cfg.Version)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	return srv, errCh
}

func (a *App) serveMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics.WriteSchema(w)
	series := []metrics.Series{{Name: "kubehero_up", Labels: map[string]string{"service": "collector"}, Value: 1}}
	if a.scanner != nil {
		series = append(series, a.scanner.Series()...)
	}
	if a.cfg.Demo {
		series = append(series, metrics.Demo()...)
	}
	metrics.WriteAll(w, series)
	metrics.Default.Write(w)
}

func (a *App) pricingClient() kuberov1connect.PricingServiceClient {
	if a.cfg.PricingEngineURL == "" {
		return nil
	}
	c, err := ship.NewPricingClient(a.cfg.PricingEngineURL, a.userAgent())
	if err != nil {
		a.log.Warn("PRICING_ENGINE_URL unusable — pricing nodes by annotation or estimate", "err", err)
		return nil
	}
	return c
}
