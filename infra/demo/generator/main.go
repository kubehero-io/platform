// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Command generator drives a synthetic three-cluster fleet through
// KubeHero's real ingest APIs — IngestPodCost (+ node cost) and the
// TelemetryService (usage, logs, profiles, flows, events) — so the
// docker-compose stack, the dashboard and the advisor run end to end on
// "live" data without a Kubernetes cluster. It is also the integration
// harness: every row it writes goes through the same validation,
// storage and rollups as a real collector's.
//
//	generator --url http://localhost:8080 --backfill 72h --interval 15s
//
// `generator workload …` instead runs a small real microservice for
// live-cluster end-to-end tests (see workload.go).
//
// Stories baked in: a checkout retry storm (spend, error logs, TLS
// handshakes in the CPU profile, egress + retransmits) that began three
// hours before start; a payments worker that OOMs every ~2h; idle A100s;
// an ETL backfill job that cannot schedule; over-provisioned gateways.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
)

type config struct {
	url, token         string
	backfill, step     time.Duration
	interval           time.Duration
	seed               uint64
	once               bool
	maxLogsPerPod      int
	backfillLogsPerPod int
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "workload" {
		if err := runWorkload(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	var cfg config
	flag.StringVar(&cfg.url, "url", envOr("CONTROL_PLANE_URL", "http://localhost:8080"), "control-plane base URL")
	flag.StringVar(&cfg.token, "token", os.Getenv("CONTROL_PLANE_TOKEN"), "bearer token (member role)")
	flag.DurationVar(&cfg.backfill, "backfill", 0, "history to write before going live (e.g. 72h)")
	flag.DurationVar(&cfg.step, "backfill-step", 5*time.Minute, "backfill resolution")
	flag.DurationVar(&cfg.interval, "interval", 15*time.Second, "live tick")
	flag.Uint64Var(&cfg.seed, "seed", 42, "deterministic fleet seed")
	flag.BoolVar(&cfg.once, "once", false, "write the backfill (and one live tick) then exit")
	flag.IntVar(&cfg.maxLogsPerPod, "logs-per-pod", 200, "cap on log lines per pod per live tick")
	flag.IntVar(&cfg.backfillLogsPerPod, "backfill-logs-per-pod", 12, "cap on log lines per pod per backfill step")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, cfg, log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("generator stopped", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type sink struct {
	cp   kuberov1connect.ControlPlaneServiceClient
	tel  kuberov1connect.TelemetryServiceClient
	cost kuberov1connect.CostServiceClient
	log  *slog.Logger
	// counters for the summary line
	pods, nodes, usage, logs, profiles, flows, events int
}

// Per-request limits the control plane enforces (split larger batches).
const (
	maxPodSamples  = 20_000
	maxNodeSamples = 1_000
	maxLogs        = 10_000
	maxFlows       = 5_000
	maxProfiles    = 500 // also bounded by 20k stack samples per request
	maxUsage       = 10_000
	maxEvents      = 5_000
)

// batch accumulates one cluster's signals across ticks so the backfill
// sends an hour of history per request instead of one request per
// five-minute step.
type batch struct {
	pods     []*kuberov1.PodCostSample
	nodes    []*kuberov1.NodeCostSample
	usage    []*kuberov1.ContainerUsage
	logs     []*kuberov1.LogEntry
	flows    []*kuberov1.Flow
	profiles []*kuberov1.Profile
	events   []*kuberov1.ClusterEvent
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	// Generous timeout: a backfill request carries an hour of history.
	httpc := &http.Client{Timeout: 2 * time.Minute}
	opts := []connect.ClientOption{connect.WithSendGzip()}
	if cfg.token != "" {
		opts = append(opts, connect.WithInterceptors(bearer(cfg.token)))
	}
	base := strings.TrimRight(cfg.url, "/")
	s := &sink{
		cp:   kuberov1connect.NewControlPlaneServiceClient(httpc, base, opts...),
		tel:  kuberov1connect.NewTelemetryServiceClient(httpc, base, opts...),
		cost: kuberov1connect.NewCostServiceClient(httpc, base, opts...),
		log:  log,
	}
	if err := waitReady(ctx, s, log); err != nil {
		return err
	}

	fleet := newFleet(cfg.seed)
	start := time.Now().UTC().Truncate(time.Second)
	r := rand.New(rand.NewPCG(cfg.seed, 7))

	if cfg.backfill > 0 {
		if s.hasHistory(ctx, cfg.backfill) {
			// A previous run (or a restart of this one) already wrote the
			// history; writing it again would double every dollar.
			log.Info("history already present, skipping backfill")
			s.summary("backfill complete")
		} else if err := backfill(ctx, s, fleet, cfg, start, r); err != nil {
			return err
		}
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		batches := map[string]*batch{}
		for _, c := range fleet {
			b := &batch{}
			collect(b, c, now, start, cfg.interval, cfg.maxLogsPerPod, true, r)
			batches[c.ID] = b
		}
		for _, c := range fleet {
			if err := s.flush(ctx, c.ID, batches[c.ID]); err != nil {
				log.Warn("live tick failed", "cluster", c.ID, "err", err)
			}
		}
		if cfg.once {
			s.summary("done")
			return nil
		}
		select {
		case <-ctx.Done():
			s.summary("stopped")
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// backfill writes cfg.backfill of history at cfg.step resolution,
// flushing each cluster once per simulated hour.
func backfill(ctx context.Context, s *sink, fleet []*cluster, cfg config, start time.Time, r *rand.Rand) error {
	from := start.Add(-cfg.backfill).Truncate(cfg.step)
	flushEvery := max(1, int(time.Hour/cfg.step))
	batches := map[string]*batch{}
	for _, c := range fleet {
		batches[c.ID] = &batch{}
	}
	steps := 0
	flushAll := func(at time.Time) error {
		for _, c := range fleet {
			if err := s.flush(ctx, c.ID, batches[c.ID]); err != nil {
				return fmt.Errorf("backfill at %s: %w", at.Format(time.RFC3339), err)
			}
			batches[c.ID] = &batch{}
		}
		return nil
	}
	for t := from.Add(cfg.step); !t.After(start); t = t.Add(cfg.step) {
		for _, c := range fleet {
			// CPU profiles every 30 simulated minutes keep the history light.
			collect(batches[c.ID], c, t, start, cfg.step, cfg.backfillLogsPerPod, t.Minute()%30 == 0, r)
		}
		steps++
		if steps%flushEvery == 0 {
			if err := flushAll(t); err != nil {
				return err
			}
			if steps%(flushEvery*12) == 0 {
				s.log.Info("backfilling", "at", t.Format(time.RFC3339), "steps", steps)
			}
		}
	}
	if err := flushAll(start); err != nil {
		return err
	}
	s.summary("backfill complete")
	return nil
}

// collect appends every signal for the interval ending at t.
func collect(b *batch, c *cluster, t, start time.Time, step time.Duration, logsPerPod int, withProfiles bool, r *rand.Rand) {
	states := snapshot(c, t, start, r)
	pods, nodes := costBatch(c, states, t, step)
	b.pods = append(b.pods, pods...)
	b.nodes = append(b.nodes, nodes...)
	b.usage = append(b.usage, usageBatch(c, states, t)...)
	b.events = append(b.events, eventBatch(c, states, t, step)...)
	b.logs = append(b.logs, logBatch(c, states, t, start, step, logsPerPod, r)...)
	b.flows = append(b.flows, flowBatch(c, states, t, start, step, r)...)
	if withProfiles {
		b.profiles = append(b.profiles, profileBatch(c, states, t, start, step)...)
	}
}

// flush sends a cluster's batch, split to the control plane's limits.
func (s *sink) flush(ctx context.Context, clusterID string, b *batch) error {
	for lo := 0; lo < len(b.pods); lo += maxPodSamples {
		req := &kuberov1.IngestPodCostRequest{ClusterId: clusterID, Samples: window(b.pods, lo, maxPodSamples)}
		if err := s.retry(ctx, "IngestPodCost", func() error {
			_, err := s.cp.IngestPodCost(ctx, connect.NewRequest(req))
			return err
		}); err != nil {
			return err
		}
	}
	for lo := 0; lo < len(b.nodes); lo += maxNodeSamples {
		req := &kuberov1.IngestPodCostRequest{ClusterId: clusterID, Nodes: window(b.nodes, lo, maxNodeSamples)}
		if err := s.retry(ctx, "IngestPodCost(nodes)", func() error {
			_, err := s.cp.IngestPodCost(ctx, connect.NewRequest(req))
			return err
		}); err != nil {
			return err
		}
	}
	s.pods += len(b.pods)
	s.nodes += len(b.nodes)

	send := func(what string, n, limit int, call func(lo, hi int) error) error {
		for lo := 0; lo < n; lo += limit {
			hi := min(n, lo+limit)
			if err := s.retry(ctx, what, func() error { return call(lo, hi) }); err != nil {
				return err
			}
		}
		return nil
	}
	if err := send("IngestUsage", len(b.usage), maxUsage, func(lo, hi int) error {
		_, err := s.tel.IngestUsage(ctx, connect.NewRequest(&kuberov1.IngestUsageRequest{ClusterId: clusterID, Usage: b.usage[lo:hi]}))
		return err
	}); err != nil {
		return err
	}
	if err := send("IngestEvents", len(b.events), maxEvents, func(lo, hi int) error {
		_, err := s.tel.IngestEvents(ctx, connect.NewRequest(&kuberov1.IngestEventsRequest{ClusterId: clusterID, Events: b.events[lo:hi]}))
		return err
	}); err != nil {
		return err
	}
	if err := send("IngestLogs", len(b.logs), maxLogs, func(lo, hi int) error {
		_, err := s.tel.IngestLogs(ctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: clusterID, Entries: b.logs[lo:hi]}))
		return err
	}); err != nil {
		return err
	}
	if err := send("IngestFlows", len(b.flows), maxFlows, func(lo, hi int) error {
		_, err := s.tel.IngestFlows(ctx, connect.NewRequest(&kuberov1.IngestFlowsRequest{ClusterId: clusterID, Flows: b.flows[lo:hi]}))
		return err
	}); err != nil {
		return err
	}
	if err := send("IngestProfiles", len(b.profiles), maxProfiles, func(lo, hi int) error {
		_, err := s.tel.IngestProfiles(ctx, connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: clusterID, Profiles: b.profiles[lo:hi]}))
		return err
	}); err != nil {
		return err
	}
	s.usage += len(b.usage)
	s.events += len(b.events)
	s.logs += len(b.logs)
	s.flows += len(b.flows)
	s.profiles += len(b.profiles)
	return nil
}

// retry runs call, backing off on errors that clear on their own
// (overload, timeouts, restarts). Validation errors fail fast.
func (s *sink) retry(ctx context.Context, what string, call func() error) error {
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		err := call()
		if err == nil {
			return nil
		}
		switch connect.CodeOf(err) {
		case connect.CodeUnavailable, connect.CodeDeadlineExceeded, connect.CodeResourceExhausted,
			connect.CodeAborted, connect.CodeUnknown, connect.CodeInternal:
		default:
			return fmt.Errorf("%s: %w", what, err)
		}
		if attempt == 6 {
			return fmt.Errorf("%s after %d attempts: %w", what, attempt, err)
		}
		s.log.Warn("retrying", "rpc", what, "attempt", attempt, "in", backoff.String(), "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 20*time.Second)
	}
}

// hasHistory reports whether this fleet's history is already stored —
// the check that makes a restarted generator skip the backfill.
func (s *sink) hasHistory(ctx context.Context, backfill time.Duration) bool {
	win := fmt.Sprintf("%dh", max(2, int(backfill.Hours())-1))
	res, err := s.cost.GetAllocation(ctx, connect.NewRequest(&kuberov1.GetAllocationRequest{
		Window: win, Aggregate: []string{"cluster"},
	}))
	if err != nil || res.Msg.GetSource() == "demo" {
		return false
	}
	start := res.Msg.GetStartUnixMs()
	for _, a := range res.Msg.GetAllocations() {
		if a.GetName() == "eks-use1-prod" && a.GetTotalCost() > 0 {
			// History covering (most of) the window, not just live ticks.
			return a.GetMinutes() > 0.5*float64(res.Msg.GetEndUnixMs()-start)/60000
		}
	}
	return false
}

func window[T any](xs []T, lo, n int) []T {
	if lo >= len(xs) {
		return nil
	}
	return xs[lo:min(len(xs), lo+n)]
}

func (s *sink) summary(msg string) {
	s.log.Info(msg, "pod_samples", s.pods, "node_samples", s.nodes, "usage", s.usage,
		"logs", s.logs, "profiles", s.profiles, "flows", s.flows, "events", s.events)
}

func waitReady(ctx context.Context, s *sink, log *slog.Logger) error {
	for attempt := 1; ; attempt++ {
		_, err := s.cp.HealthCheck(ctx, connect.NewRequest(&kuberov1.HealthCheckRequest{}))
		if err == nil {
			return nil
		}
		if attempt%5 == 1 {
			log.Info("waiting for control plane", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func bearer(token string) connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	}
}
