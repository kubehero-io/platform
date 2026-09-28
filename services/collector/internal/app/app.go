// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package app wires the collector's pipelines into one process and owns
// its lifecycle: start order (informers before consumers), degraded
// modes (no control plane, no kubelet stats, no eBPF, no leader), and a
// shutdown that drains the ship queues before exiting and only then
// checkpoints log offsets, so a rolling restart neither loses nor
// duplicates what was already read.
//
// Memory budget during a control-plane outage (queues full, oldest
// dropped beyond): cost 8 MiB (~20 min of 5s scans on a busy node),
// usage 4 MiB, events 4 MiB, logs --logs-max-buffer-bytes (64 MiB),
// profiles 16 MiB, flows 16 MiB — about 112 MiB on top of the ~50 MiB
// steady state, so a 256–512 MiB limit never OOMs from backpressure.
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
	"github.com/kubehero-io/platform/services/collector/internal/ebpf"
	"github.com/kubehero-io/platform/services/collector/internal/events"
	"github.com/kubehero-io/platform/services/collector/internal/ingest"
	"github.com/kubehero-io/platform/services/collector/internal/kube"
	"github.com/kubehero-io/platform/services/collector/internal/kubeletstats"
	"github.com/kubehero-io/platform/services/collector/internal/leader"
	"github.com/kubehero-io/platform/services/collector/internal/logs"
	"github.com/kubehero-io/platform/services/collector/internal/metrics"
	"github.com/kubehero-io/platform/services/collector/internal/profiles"
	"github.com/kubehero-io/platform/services/collector/internal/ship"
	"github.com/kubehero-io/platform/services/collector/internal/usage"
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
	UsageInterval      time.Duration
	KubeletURL         string
	KubeletInsecureTLS bool
	KubeletStatsTTL    time.Duration

	// Events enables health-event detection (node-local container
	// status; cluster-scoped sources on the leader).
	Events bool
	// LeaderElect runs cluster-scoped duties only on the holder of the
	// LeaseName Lease in PodNamespace. Disabling it runs them on EVERY
	// collector (only sensible for a single-node or dev setup).
	LeaderElect bool
	LeaseName   string

	Logs     LogsConfig
	Profiles ProfilesConfig
	EBPF     EBPFConfig

	// ShutdownTimeout bounds the final queue flush.
	ShutdownTimeout time.Duration

	// Client overrides the Kubernetes client (tests).
	Client kubernetes.Interface
	// Stats overrides the kubelet stats provider (tests).
	Stats kubeletstats.Provider
}

// LogsConfig configures container log tailing.
type LogsConfig struct {
	Enabled           bool
	Root              string
	PositionsFile     string
	From              string // end | start
	RateLimit         float64
	Burst             int
	ExcludeNamespaces []string
	PodLabels         []string
	MaxBufferBytes    int
}

// ProfilesConfig configures pprof scraping.
type ProfilesConfig struct {
	Enabled    bool
	Interval   time.Duration
	CPUSeconds int
}

// EBPFConfig configures the kernel telemetry (internal/ebpf).
type EBPFConfig struct {
	Enabled       bool
	Netflow       bool
	Profiler      bool
	ProfileHz     int
	CgroupRoot    string
	FlushInterval time.Duration
}

// App is a running collector.
type App struct {
	cfg   Config
	log   *slog.Logger
	kube  kubernetes.Interface
	local *kube.LocalCache

	ship *ship.Client
	// scanner is read by /metrics, which serves before the scanner exists.
	scanner atomic.Pointer[ingest.Scanner]
	ready   atomic.Bool

	flushers   []func(context.Context)
	afterFlush []func()
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
	if cfg.UsageInterval <= 0 {
		cfg.UsageInterval = 30 * time.Second
	}
	if cfg.LeaseName == "" {
		cfg.LeaseName = "kubehero-collector"
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
		a.log.Warn("CONTROL_PLANE_URL unset — nothing is shipped: cost is computed and exposed on /metrics only; usage, events, logs, profiles and eBPF are disabled")
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
			return a.shutdown(srv)
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
		q := ship.NewQueue(ship.QueueConfig{Signal: "cost", MaxItems: 50_000, MaxBytes: 8 << 20, Timeout: 10 * time.Second}, a.ship.SendPodCost, a.log)
		a.startQueue(ctx, &wg, q.Run, q.Flush)
		emitCost = func(r *kuberov1.IngestPodCostRequest) {
			q.Enqueue(ship.Batch[*kuberov1.IngestPodCostRequest]{Req: r, Items: len(r.Samples) + len(r.Nodes), Bytes: proto.Size(r)})
		}
	}
	pricingClient := a.pricingClient()
	scanner := ingest.New(ingest.Config{
		ClusterID: a.cfg.ClusterID, NodeName: a.cfg.NodeName, Interval: a.cfg.ScanInterval, Logger: a.log,
	}, a.local, owners, stats, ingest.NewPricer(pricingClient, a.log), emitCost)
	a.scanner.Store(scanner)
	a.goLoop(ctx, &wg, scanner.Run)

	if a.ship != nil {
		a.startUsage(ctx, &wg, owners, stats)
		if a.cfg.Events {
			a.startEvents(ctx, &wg, owners)
		}
		if a.cfg.Logs.Enabled {
			a.startLogs(ctx, &wg, owners)
		}
		var profileQ *ship.Queue[*kuberov1.IngestProfilesRequest]
		if a.cfg.Profiles.Enabled || (a.cfg.EBPF.Enabled && a.cfg.EBPF.Profiler) {
			profileQ = ship.NewQueue(ship.QueueConfig{Signal: "profiles", MaxItems: 20_000, MaxBytes: 16 << 20, Timeout: 30 * time.Second}, a.ship.SendProfiles, a.log)
			a.startQueue(ctx, &wg, profileQ.Run, profileQ.Flush)
		}
		if a.cfg.Profiles.Enabled {
			a.startProfiles(ctx, &wg, owners, profileQ)
		}
		if a.cfg.EBPF.Enabled {
			a.startEBPF(ctx, &wg, owners, profileQ)
		}
	}

	select {
	case <-ctx.Done():
	case err := <-srvErr:
		cancel()
		wg.Wait()
		return errors.Join(err, a.shutdown(srv))
	}
	a.log.Info("shutting down")
	wg.Wait()
	return a.shutdown(srv)
}

// shutdown drains the ship queues (bounded by ShutdownTimeout), runs the
// post-flush hooks (log checkpoints, which must only record what the
// flush actually delivered), and stops the HTTP server.
func (a *App) shutdown(srv *http.Server) error {
	fctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()
	var fw sync.WaitGroup
	for _, f := range a.flushers {
		fw.Add(1)
		go func() { defer fw.Done(); f(fctx) }()
	}
	fw.Wait()
	for _, f := range a.afterFlush {
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

func (a *App) startUsage(ctx context.Context, wg *sync.WaitGroup, owners *kube.OwnerResolver, stats kubeletstats.Provider) {
	q := ship.NewQueue(ship.QueueConfig{Signal: "usage", MaxItems: 50_000, MaxBytes: 4 << 20}, a.ship.SendUsage, a.log)
	a.startQueue(ctx, wg, q.Run, q.Flush)
	sampler := usage.New(usage.Config{
		ClusterID: a.cfg.ClusterID, NodeName: a.cfg.NodeName, Interval: a.cfg.UsageInterval, Logger: a.log,
	}, a.local, owners, stats, func(r *kuberov1.IngestUsageRequest) {
		q.Enqueue(ship.Batch[*kuberov1.IngestUsageRequest]{Req: r, Items: len(r.Usage), Bytes: proto.Size(r)})
	})
	a.goLoop(ctx, wg, sampler.Run)
}

// startEvents runs node-local status detection on every collector and
// the cluster-scoped sources on the elected leader only.
func (a *App) startEvents(ctx context.Context, wg *sync.WaitGroup, owners *kube.OwnerResolver) {
	q := ship.NewQueue(ship.QueueConfig{Signal: "events", MaxItems: 20_000, MaxBytes: 4 << 20}, a.ship.SendEvents, a.log)
	a.startQueue(ctx, wg, q.Run, q.Flush)
	batcher := events.NewBatcher(a.cfg.ClusterID, 5*time.Second, func(r *kuberov1.IngestEventsRequest) {
		q.Enqueue(ship.Batch[*kuberov1.IngestEventsRequest]{Req: r, Items: len(r.Events), Bytes: proto.Size(r)})
	})
	a.goLoop(ctx, wg, batcher.Run)

	detector := events.NewStatusDetector(a.local, owners)
	a.goLoop(ctx, wg, func(ctx context.Context) {
		tickLoop(ctx, a.cfg.ScanInterval, func(now time.Time) {
			batcher.Add("status", detector.Detect(ctx, now)...)
		})
	})

	duties := func(lctx context.Context) {
		events.NewClusterWatcher(events.ClusterConfig{Logger: a.log}, a.kube, owners, batcher.Add).Run(lctx)
	}
	if !a.cfg.LeaderElect {
		a.log.Warn("--leader-elect=false — cluster-scoped event sources run on this collector unconditionally")
		a.goLoop(ctx, wg, duties)
		return
	}
	a.goLoop(ctx, wg, func(ctx context.Context) {
		leader.Run(ctx, leader.Config{
			Client: a.kube, Namespace: a.cfg.PodNamespace, Name: a.cfg.LeaseName, Identity: a.cfg.PodName, Logger: a.log,
		}, duties)
	})
}

// startLogs tails /var/log/pods for this node's containers.
func (a *App) startLogs(ctx context.Context, wg *sync.WaitGroup, owners *kube.OwnerResolver) {
	lc := a.cfg.Logs
	maxBuf := lc.MaxBufferBytes
	if maxBuf <= 0 {
		maxBuf = 64 << 20
	}
	q := ship.NewQueue(ship.QueueConfig{Signal: "logs", MaxItems: 2_000_000, MaxBytes: maxBuf, Timeout: 20 * time.Second}, a.ship.SendLogs, a.log)
	a.startQueue(ctx, wg, q.Run, q.Flush)
	tailer := logs.New(logs.Config{
		Root:              lc.Root,
		PositionsFile:     lc.PositionsFile,
		FromStart:         lc.From == "start",
		RateLimit:         lc.RateLimit,
		Burst:             lc.Burst,
		ExcludeNamespaces: lc.ExcludeNamespaces,
		PodLabels:         lc.PodLabels,
		ClusterID:         a.cfg.ClusterID,
		NodeName:          a.cfg.NodeName,
		Logger:            a.log,
	}, a.local, owners, func(r *kuberov1.IngestLogsRequest, done func(ship.Outcome)) {
		q.Enqueue(ship.Batch[*kuberov1.IngestLogsRequest]{Req: r, Items: len(r.Entries), Bytes: proto.Size(r), Done: done})
	})
	a.goLoop(ctx, wg, tailer.Run)
	a.afterFlush = append(a.afterFlush, tailer.Checkpoint)
}

func (a *App) startProfiles(ctx context.Context, wg *sync.WaitGroup, owners *kube.OwnerResolver, q *ship.Queue[*kuberov1.IngestProfilesRequest]) {
	sc := profiles.New(profiles.Config{
		ClusterID: a.cfg.ClusterID, NodeName: a.cfg.NodeName,
		Interval: a.cfg.Profiles.Interval, CPUSeconds: a.cfg.Profiles.CPUSeconds, Logger: a.log,
	}, a.local, owners, func(r *kuberov1.IngestProfilesRequest) { enqueueProfiles(q, r) })
	a.goLoop(ctx, wg, sc.Run)
}

func enqueueProfiles(q *ship.Queue[*kuberov1.IngestProfilesRequest], r *kuberov1.IngestProfilesRequest) {
	q.Enqueue(ship.Batch[*kuberov1.IngestProfilesRequest]{Req: r, Items: len(r.Profiles), Bytes: proto.Size(r)})
}

// maxFlowsPerRequest matches the control plane's per-request limit.
const maxFlowsPerRequest = 5000

// startEBPF attaches the kernel programs. Netflow needs the cluster-wide
// IP index (pod / service / node informers on every node). Those
// informers start only after the programs attached: a host without eBPF
// support must not list every pod in the cluster just to throw the
// result away (N nodes × a full list on every rollout). The first drain
// runs a FlushInterval after attach, by which time the index has synced;
// anything drained earlier is attributed "external" at worst.
func (a *App) startEBPF(ctx context.Context, wg *sync.WaitGroup, owners *kube.OwnerResolver, profileQ *ship.Queue[*kuberov1.IngestProfilesRequest]) {
	ec := a.cfg.EBPF
	ectx, ecancel := context.WithCancel(ctx)
	var client kubernetes.Interface
	if ec.Netflow {
		client = a.kube
	}
	res, err := kube.NewResolver(client, a.local, owners)
	if err != nil {
		ecancel()
		a.log.Warn("eBPF resolver setup failed — kernel telemetry disabled", "err", err)
		return
	}
	flowQ := ship.NewQueue(ship.QueueConfig{Signal: "flows", MaxItems: 200_000, MaxBytes: 16 << 20, Timeout: 20 * time.Second}, a.ship.SendFlows, a.log)
	a.startQueue(ctx, wg, flowQ.Run, flowQ.Flush)

	err = ebpf.Start(ectx, ebpf.Config{
		Netflow:       ec.Netflow,
		Profiler:      ec.Profiler,
		CgroupRoot:    ec.CgroupRoot,
		FlushInterval: ec.FlushInterval,
		ProfileHz:     ec.ProfileHz,
		Resolver:      res,
		Logger:        a.log.With("component", "ebpf"),
		EmitFlows: func(_ context.Context, flows []*kuberov1.Flow) error {
			for len(flows) > 0 {
				n := min(len(flows), maxFlowsPerRequest)
				// Copy: the drain loop may reuse its slice once we return,
				// while this request waits in the queue.
				r := &kuberov1.IngestFlowsRequest{ClusterId: a.cfg.ClusterID, Flows: append([]*kuberov1.Flow(nil), flows[:n]...)}
				flowQ.Enqueue(ship.Batch[*kuberov1.IngestFlowsRequest]{Req: r, Items: n, Bytes: proto.Size(r)})
				flows = flows[n:]
			}
			return nil
		},
		EmitProfiles: func(_ context.Context, ps []*kuberov1.Profile) error {
			if profileQ == nil {
				return nil
			}
			for _, r := range profiles.Split(a.cfg.ClusterID, ps) {
				enqueueProfiles(profileQ, r)
			}
			return nil
		},
	})
	switch {
	case errors.Is(err, ebpf.ErrUnsupported):
		ecancel()
		a.log.Info("eBPF kernel telemetry unavailable on this host — continuing without network flows and CPU sampling", "reason", err.Error())
		return
	case err != nil:
		ecancel()
		a.log.Warn("eBPF programs failed to start — continuing without kernel telemetry", "err", err)
		return
	}
	if ec.Netflow {
		metrics.EBPFStatus.With("netflow").Set(1)
		go func() {
			if err := res.Start(ectx); err != nil && ectx.Err() == nil {
				a.log.Warn("cluster-wide pod/service/node cache did not sync — flow peers will read as external", "err", err)
			}
		}()
	}
	if ec.Profiler {
		metrics.EBPFStatus.With("profiler").Set(1)
	}
	// ebpf.Stats() is published on scrape — see serveMetrics.
	a.goLoop(ctx, wg, func(ctx context.Context) {
		<-ctx.Done()
		ecancel()
	})
}

// tickLoop calls fn every interval until ctx ends.
func tickLoop(ctx context.Context, interval time.Duration, fn func(time.Time)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			fn(now)
		}
	}
}

func (a *App) statsProvider() (kubeletstats.Provider, error) {
	if a.cfg.Stats != nil {
		return a.cfg.Stats, nil
	}
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
	if sc := a.scanner.Load(); sc != nil {
		series = append(series, sc.Series()...)
	}
	if a.cfg.Demo {
		series = append(series, metrics.Demo()...)
	}
	metrics.WriteAll(w, series)
	// Kernel telemetry counters (zeros when eBPF isn't running).
	st := ebpf.Stats()
	metrics.PublishEBPF(
		map[string]bool{"netflow": st.NetflowAttached, "retransmits": st.RetransmitsAttached, "profiler": st.ProfilerAttached},
		map[string]uint64{"flows_emitted": st.FlowsEmitted, "flow_entries_drained": st.FlowEntriesDrained,
			"profiles_emitted": st.ProfilesEmitted, "samples_drained": st.SamplesDrained,
			"samples_unattributed": st.SamplesUnattributed, "stacks_capped": st.StacksCapped,
			"stacks_lost": st.StacksLost, "drain_errors": st.DrainErrors, "map_full_events": st.MapFullEvents})
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
