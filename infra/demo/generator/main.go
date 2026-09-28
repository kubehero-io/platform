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
	cp  kuberov1connect.ControlPlaneServiceClient
	tel kuberov1connect.TelemetryServiceClient
	log *slog.Logger
	// counters for the summary line
	pods, nodes, usage, logs, profiles, flows, events int
}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	httpc := &http.Client{Timeout: 30 * time.Second}
	opts := []connect.ClientOption{connect.WithSendGzip()}
	if cfg.token != "" {
		opts = append(opts, connect.WithInterceptors(bearer(cfg.token)))
	}
	base := strings.TrimRight(cfg.url, "/")
	s := &sink{
		cp:  kuberov1connect.NewControlPlaneServiceClient(httpc, base, opts...),
		tel: kuberov1connect.NewTelemetryServiceClient(httpc, base, opts...),
		log: log,
	}
	if err := waitReady(ctx, s, log); err != nil {
		return err
	}

	fleet := newFleet(cfg.seed)
	start := time.Now().UTC().Truncate(time.Second)
	r := rand.New(rand.NewPCG(cfg.seed, 7))

	if cfg.backfill > 0 {
		from := start.Add(-cfg.backfill).Truncate(cfg.step)
		steps := 0
		for t := from.Add(cfg.step); !t.After(start); t = t.Add(cfg.step) {
			if err := tick(ctx, s, fleet, t, start, cfg.step, cfg.backfillLogsPerPod, t.Minute()%30 == 0, r); err != nil {
				return fmt.Errorf("backfill at %s: %w", t.Format(time.RFC3339), err)
			}
			steps++
			if steps%48 == 0 {
				log.Info("backfilling", "at", t.Format(time.RFC3339), "steps", steps)
			}
		}
		s.summary("backfill complete")
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()
	for {
		now := time.Now().UTC()
		if err := tick(ctx, s, fleet, now, start, cfg.interval, cfg.maxLogsPerPod, true, r); err != nil {
			log.Warn("live tick failed", "err", err)
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

// tick writes every signal for every cluster for the interval ending at t.
func tick(ctx context.Context, s *sink, fleet []*cluster, t, start time.Time, step time.Duration, logsPerPod int, withProfiles bool, r *rand.Rand) error {
	for _, c := range fleet {
		states := snapshot(c, t, start, r)
		pods, nodes := costBatch(c, states, t, step)
		if _, err := s.cp.IngestPodCost(ctx, connect.NewRequest(&kuberov1.IngestPodCostRequest{ClusterId: c.ID, Samples: pods, Nodes: nodes})); err != nil {
			return fmt.Errorf("IngestPodCost %s: %w", c.ID, err)
		}
		s.pods += len(pods)
		s.nodes += len(nodes)

		if u := usageBatch(c, states, t); len(u) > 0 {
			if _, err := s.tel.IngestUsage(ctx, connect.NewRequest(&kuberov1.IngestUsageRequest{ClusterId: c.ID, Usage: u})); err != nil {
				return fmt.Errorf("IngestUsage %s: %w", c.ID, err)
			}
			s.usage += len(u)
		}
		if ev := eventBatch(c, states, t, step); len(ev) > 0 {
			if _, err := s.tel.IngestEvents(ctx, connect.NewRequest(&kuberov1.IngestEventsRequest{ClusterId: c.ID, Events: ev})); err != nil {
				return fmt.Errorf("IngestEvents %s: %w", c.ID, err)
			}
			s.events += len(ev)
		}
		logs := logBatch(c, states, t, start, step, logsPerPod, r)
		for len(logs) > 0 {
			n := min(len(logs), 5000)
			if _, err := s.tel.IngestLogs(ctx, connect.NewRequest(&kuberov1.IngestLogsRequest{ClusterId: c.ID, Entries: logs[:n]})); err != nil {
				return fmt.Errorf("IngestLogs %s: %w", c.ID, err)
			}
			s.logs += n
			logs = logs[n:]
		}
		if f := flowBatch(c, states, t, start, step, r); len(f) > 0 {
			if _, err := s.tel.IngestFlows(ctx, connect.NewRequest(&kuberov1.IngestFlowsRequest{ClusterId: c.ID, Flows: f})); err != nil {
				return fmt.Errorf("IngestFlows %s: %w", c.ID, err)
			}
			s.flows += len(f)
		}
		if withProfiles {
			if p := profileBatch(c, states, t, start, step); len(p) > 0 {
				if _, err := s.tel.IngestProfiles(ctx, connect.NewRequest(&kuberov1.IngestProfilesRequest{ClusterId: c.ID, Profiles: p})); err != nil {
					return fmt.Errorf("IngestProfiles %s: %w", c.ID, err)
				}
				s.profiles += len(p)
			}
		}
	}
	return nil
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
