// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package telemetry is the write path for every signal beyond pod
// cost: container logs, profiles, eBPF network flows, container usage
// and cluster events (TelemetryService), plus the entry points the
// compatibility shims (Loki push, OTLP logs, Pyroscope ingest) call
// after translating their wire formats.
//
// Each RPC validates its batch (limits, timestamps, label hygiene —
// see convert.go), resolves the cluster, and queues the rows on a
// per-table batcher that writes large native inserts at most about
// once a second. RPC latency is therefore the validation cost, not a
// ClickHouse round trip; a full queue answers ResourceExhausted so
// agents back off, and shutdown drains the queues. Without ClickHouse
// the RPCs accept and drop (reporting every row as dropped), exactly
// like IngestPodCost.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/batcher"
	"github.com/kubehero-io/platform/services/control-plane/internal/clickhouse"
)

// Limits caps one request. Larger batches are rejected with
// InvalidArgument — the agent should split them — rather than
// half-accepted.
type Limits struct {
	Logs     int
	Flows    int
	Profiles int
	Samples  int // stack samples across every profile of a request
	Usage    int
	Events   int
}

// DefaultLimits are the per-request caps.
var DefaultLimits = Limits{Logs: 10_000, Flows: 5_000, Profiles: 2_000, Samples: 20_000, Usage: 10_000, Events: 5_000}

// Writer stores rows; *clickhouse.SignalWriter implements it.
type Writer interface {
	WriteLogs(ctx context.Context, rows []clickhouse.LogRow) error
	WriteProfileSamples(ctx context.Context, rows []clickhouse.ProfileSampleRow) error
	WriteProfileStacks(ctx context.Context, rows []clickhouse.ProfileStackRow) error
	WriteFlows(ctx context.Context, rows []clickhouse.FlowRow) error
	WriteUsage(ctx context.Context, rows []clickhouse.UsageRow) error
	WriteEvents(ctx context.Context, rows []clickhouse.EventRow) error
}

// Options configures a Service.
type Options struct {
	// Writer stores rows; nil = ClickHouse not configured (accept and
	// drop).
	Writer Writer
	Limits Limits
	// Pricing prices flows; zero value = per-cloud list prices.
	Pricing *NetPricing
	// Clouds resolves each cluster's cloud for flow pricing.
	Clouds *CloudResolver
	// LogQueueBytes bounds buffered log bytes (default 256 MiB).
	LogQueueBytes int64
	// FlushInterval overrides the 1s batching interval (tests).
	FlushInterval time.Duration
	Log           *slog.Logger
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Service implements kuberov1connect.TelemetryServiceHandler.
type Service struct {
	limits  Limits
	pricing NetPricing
	clouds  *CloudResolver
	log     *slog.Logger
	now     func() time.Time

	// nil when ClickHouse is not configured.
	logs    *batcher.Batcher[clickhouse.LogRow]
	samples *batcher.Batcher[clickhouse.ProfileSampleRow]
	stacks  *batcher.Batcher[clickhouse.ProfileStackRow]
	flows   *batcher.Batcher[clickhouse.FlowRow]
	usage   *batcher.Batcher[clickhouse.UsageRow]
	events  *batcher.Batcher[clickhouse.EventRow]

	stackCache *StackCache
}

var _ kuberov1connect.TelemetryServiceHandler = (*Service)(nil)

// New builds the service. Call Run to start its writers.
func New(o Options) *Service {
	s := &Service{
		limits:     o.Limits,
		pricing:    DefaultNetPricing(),
		clouds:     o.Clouds,
		log:        o.Log,
		now:        o.Now,
		stackCache: NewStackCache(200_000, 24*time.Hour),
	}
	if s.limits == (Limits{}) {
		s.limits = DefaultLimits
	}
	if o.Pricing != nil {
		s.pricing = *o.Pricing
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if o.Writer == nil {
		return s
	}
	w := o.Writer
	interval := o.FlushInterval
	logBytes := o.LogQueueBytes
	if logBytes <= 0 {
		logBytes = 256 << 20
	}
	cfg := func(name string, flushRows int) batcher.Config {
		return batcher.Config{Name: name, FlushRows: flushRows, FlushInterval: interval, Log: s.log}
	}
	logCfg := cfg("logs", 100_000)
	logCfg.MaxQueueRows = 2_000_000
	logCfg.MaxQueueBytes = logBytes
	logCfg.Workers = 2
	s.logs = batcher.New(logCfg, w.WriteLogs, logRowSize)
	s.samples = batcher.New(cfg("profile_samples", 100_000), w.WriteProfileSamples, nil)
	s.stacks = batcher.New(cfg("profile_stacks", 50_000), func(ctx context.Context, rows []clickhouse.ProfileStackRow) error {
		err := w.WriteProfileStacks(ctx, rows)
		if err != nil {
			// Written-hash cache must not claim stacks that failed.
			hashes := make([]uint64, len(rows))
			for i := range rows {
				hashes[i] = rows[i].StackHash
			}
			s.stackCache.Forget(hashes)
		}
		return err
	}, stackRowSize)
	s.flows = batcher.New(cfg("net_flows", 50_000), w.WriteFlows, nil)
	s.usage = batcher.New(cfg("container_usage", 50_000), w.WriteUsage, nil)
	s.events = batcher.New(cfg("cluster_events", 20_000), w.WriteEvents, nil)
	return s
}

func logRowSize(r clickhouse.LogRow) int {
	n := 256 + len(r.Body) + len(r.Pod) + len(r.TraceID)
	for k, v := range r.Labels {
		n += len(k) + len(v) + 16
	}
	return n
}

func stackRowSize(r clickhouse.ProfileStackRow) int {
	n := 64
	for _, f := range r.Frames {
		n += len(f) + 16
	}
	return n
}

// Enabled reports whether rows are stored (ClickHouse configured).
func (s *Service) Enabled() bool { return s.logs != nil }

// Run starts the writers; they drain and stop when ctx ends.
func (s *Service) Run(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	s.logs.Run(ctx)
	s.samples.Run(ctx)
	s.stacks.Run(ctx)
	s.flows.Run(ctx)
	s.usage.Run(ctx)
	s.events.Run(ctx)
}

// Done is closed when every writer has drained after shutdown.
func (s *Service) Done() <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if !s.Enabled() {
			return
		}
		<-s.logs.Done()
		<-s.samples.Done()
		<-s.stacks.Done()
		<-s.flows.Done()
		<-s.usage.Done()
		<-s.events.Done()
	}()
	return done
}

// Flush writes everything queued and waits (read-your-writes for tests
// and the compat shims' synchronous paths).
func (s *Service) Flush(ctx context.Context) error {
	if !s.Enabled() {
		return nil
	}
	// Stacks before samples: a sample must never be visible before the
	// stack it references.
	return errors.Join(
		s.stacks.Flush(ctx), s.samples.Flush(ctx), s.logs.Flush(ctx),
		s.flows.Flush(ctx), s.usage.Flush(ctx), s.events.Flush(ctx))
}

// Stats snapshots every writer's counters.
func (s *Service) Stats() []batcher.Stats {
	if !s.Enabled() {
		return nil
	}
	return []batcher.Stats{s.logs.Stats(), s.samples.Stats(), s.stacks.Stats(),
		s.flows.Stats(), s.usage.Stats(), s.events.Stats()}
}

var clusterIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// ResolveCluster decides which cluster a write is attributed to: the
// cluster the request names, else the cluster of the enrollment token
// the caller authenticated with. A cluster-scoped token may not write
// another cluster's data. "" (with a nil error) means no cluster could
// be determined; callers drop the rows.
func ResolveCluster(ctx context.Context, requested string) (string, error) {
	requested = strings.TrimSpace(requested)
	scoped := auth.PrincipalFromContext(ctx).ClusterID
	if scoped != "" && requested != "" && requested != scoped {
		return "", connect.NewError(connect.CodePermissionDenied,
			fmt.Errorf("the enrollment token authenticates cluster %q and cannot write data for cluster %q (set CLUSTER_ID to %q or omit cluster_id)", scoped, requested, scoped))
	}
	cluster := requested
	if cluster == "" {
		cluster = scoped
	}
	if cluster != "" && !clusterIDRE.MatchString(cluster) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("cluster_id %q: want 1-128 characters of [A-Za-z0-9._:-]", cluster))
	}
	return cluster, nil
}

// queueErr maps a batcher refusal to the RPC status agents act on.
func queueErr(table string, err error) error {
	switch {
	case errors.Is(err, batcher.ErrFull):
		return connect.NewError(connect.CodeResourceExhausted,
			fmt.Errorf("%s ingest queue is full; retry with backoff", table))
	case errors.Is(err, batcher.ErrClosed):
		return connect.NewError(connect.CodeUnavailable,
			fmt.Errorf("%s ingest is shutting down; retry", table))
	}
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%s: %w", table, err))
}

func tooMany(what string, got, limit int) error {
	return connect.NewError(connect.CodeInvalidArgument,
		fmt.Errorf("at most %d %s per request, got %d; split the batch", limit, what, got))
}

func (s *Service) clock() clock { return clock{now: s.now().UTC()} }

// ─── Logs ────────────────────────────────────────────────────────────────

// IngestLogs stores container log lines.
func (s *Service) IngestLogs(ctx context.Context, req *connect.Request[kuberov1.IngestLogsRequest]) (*connect.Response[kuberov1.IngestLogsResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	entries := req.Msg.GetEntries()
	if len(entries) > s.limits.Logs {
		return nil, tooMany("log entries", len(entries), s.limits.Logs)
	}
	cluster, err := ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	accepted, dropped, err := s.WriteLogs(cluster, entries)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kuberov1.IngestLogsResponse{Accepted: int32(accepted), Dropped: int32(dropped)}), nil
}

// WriteLogs validates and queues entries for an already-resolved
// cluster — the entry point the Loki and OTLP shims share with
// IngestLogs. It enforces no per-call entry limit (callers chunk).
func (s *Service) WriteLogs(cluster string, entries []*kuberov1.LogEntry) (accepted, dropped int, err error) {
	if !s.Enabled() || cluster == "" {
		return 0, len(entries), nil
	}
	c := s.clock()
	rows := make([]clickhouse.LogRow, 0, len(entries))
	for _, e := range entries {
		if r, ok := c.LogRow(cluster, e); ok {
			rows = append(rows, r)
		}
	}
	if err := s.logs.Add(rows); err != nil {
		return 0, 0, queueErr("logs", err)
	}
	return len(rows), len(entries) - len(rows), nil
}

// ─── Profiles ────────────────────────────────────────────────────────────

// IngestProfiles stores aggregated stack samples. accepted counts
// profiles, samples counts stored stack rows, dropped counts discarded
// stack samples (every sample of a rejected profile included).
func (s *Service) IngestProfiles(ctx context.Context, req *connect.Request[kuberov1.IngestProfilesRequest]) (*connect.Response[kuberov1.IngestProfilesResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	profiles := req.Msg.GetProfiles()
	if len(profiles) > s.limits.Profiles {
		return nil, tooMany("profiles", len(profiles), s.limits.Profiles)
	}
	total := 0
	for _, p := range profiles {
		total += len(p.GetSamples())
	}
	if total > s.limits.Samples {
		return nil, tooMany("stack samples", total, s.limits.Samples)
	}
	cluster, err := ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	res, err := s.WriteProfiles(cluster, profiles)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&kuberov1.IngestProfilesResponse{
		Accepted: int32(res.Profiles), Samples: int32(res.Samples), Dropped: int32(res.Dropped),
	}), nil
}

// ProfileWrite summarises one WriteProfiles call.
type ProfileWrite struct {
	Profiles int // accepted profiles
	Samples  int // stored stack rows
	Dropped  int // discarded stack samples
}

// WriteProfiles validates and queues profiles for a resolved cluster
// (shared with the Pyroscope shim). Stacks already written recently
// are not rewritten.
func (s *Service) WriteProfiles(cluster string, profiles []*kuberov1.Profile) (ProfileWrite, error) {
	var res ProfileWrite
	if !s.Enabled() || cluster == "" {
		for _, p := range profiles {
			res.Dropped += max(1, len(p.GetSamples()))
		}
		return res, nil
	}
	c := s.clock()
	var samples []clickhouse.ProfileSampleRow
	var stacks []clickhouse.ProfileStackRow
	for _, p := range profiles {
		pr, ok := c.ProfileRows(cluster, p)
		res.Dropped += pr.Dropped
		if !ok || len(pr.Samples) == 0 {
			continue
		}
		res.Profiles++
		samples = append(samples, pr.Samples...)
		for _, st := range pr.Stacks {
			if s.stackCache.ShouldWrite(st.StackHash, c.now) {
				stacks = append(stacks, st)
			}
		}
	}
	// Queue stacks first; if they are refused, forget them and refuse
	// the samples too so no sample points at a missing stack.
	if err := s.stacks.Add(stacks); err != nil {
		hashes := make([]uint64, len(stacks))
		for i := range stacks {
			hashes[i] = stacks[i].StackHash
		}
		s.stackCache.Forget(hashes)
		return ProfileWrite{}, queueErr("profile_stacks", err)
	}
	if err := s.samples.Add(samples); err != nil {
		return ProfileWrite{}, queueErr("profile_samples", err)
	}
	res.Samples = len(samples)
	return res, nil
}

// ─── Flows ───────────────────────────────────────────────────────────────

// IngestFlows stores eBPF flows, priced at ingest.
func (s *Service) IngestFlows(ctx context.Context, req *connect.Request[kuberov1.IngestFlowsRequest]) (*connect.Response[kuberov1.IngestFlowsResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	flows := req.Msg.GetFlows()
	if len(flows) > s.limits.Flows {
		return nil, tooMany("flows", len(flows), s.limits.Flows)
	}
	cluster, err := ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	if !s.Enabled() || cluster == "" {
		return connect.NewResponse(&kuberov1.IngestFlowsResponse{Dropped: int32(len(flows))}), nil
	}
	c := s.clock()
	var zones []string
	for _, f := range flows {
		if z := f.GetSrc().GetZone(); z != "" {
			zones = append(zones, z)
			break
		}
	}
	cloud := s.clouds.Cloud(ctx, cluster, zones...)
	rows := make([]clickhouse.FlowRow, 0, len(flows))
	for _, f := range flows {
		if r, ok := c.FlowRow(cluster, cloud, s.pricing, f); ok {
			rows = append(rows, r)
		}
	}
	if err := s.flows.Add(rows); err != nil {
		return nil, queueErr("net_flows", err)
	}
	return connect.NewResponse(&kuberov1.IngestFlowsResponse{
		Accepted: int32(len(rows)), Dropped: int32(len(flows) - len(rows)),
	}), nil
}

// ─── Usage ───────────────────────────────────────────────────────────────

// IngestUsage stores per-container utilisation vs requests/limits.
func (s *Service) IngestUsage(ctx context.Context, req *connect.Request[kuberov1.IngestUsageRequest]) (*connect.Response[kuberov1.IngestUsageResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	in := req.Msg.GetUsage()
	if len(in) > s.limits.Usage {
		return nil, tooMany("usage samples", len(in), s.limits.Usage)
	}
	cluster, err := ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	if !s.Enabled() || cluster == "" {
		return connect.NewResponse(&kuberov1.IngestUsageResponse{Dropped: int32(len(in))}), nil
	}
	c := s.clock()
	rows := make([]clickhouse.UsageRow, 0, len(in))
	for _, u := range in {
		if r, ok := c.UsageRow(cluster, u); ok {
			rows = append(rows, r)
		}
	}
	if err := s.usage.Add(rows); err != nil {
		return nil, queueErr("container_usage", err)
	}
	return connect.NewResponse(&kuberov1.IngestUsageResponse{
		Accepted: int32(len(rows)), Dropped: int32(len(in) - len(rows)),
	}), nil
}

// ─── Events ──────────────────────────────────────────────────────────────

// IngestEvents stores Kubernetes health events.
func (s *Service) IngestEvents(ctx context.Context, req *connect.Request[kuberov1.IngestEventsRequest]) (*connect.Response[kuberov1.IngestEventsResponse], error) {
	if err := auth.Require(ctx, auth.RoleMember); err != nil {
		return nil, err
	}
	in := req.Msg.GetEvents()
	if len(in) > s.limits.Events {
		return nil, tooMany("events", len(in), s.limits.Events)
	}
	cluster, err := ResolveCluster(ctx, req.Msg.GetClusterId())
	if err != nil {
		return nil, err
	}
	if !s.Enabled() || cluster == "" {
		return connect.NewResponse(&kuberov1.IngestEventsResponse{Dropped: int32(len(in))}), nil
	}
	c := s.clock()
	rows := make([]clickhouse.EventRow, 0, len(in))
	for _, e := range in {
		if r, ok := c.EventRow(cluster, e); ok {
			rows = append(rows, r)
		}
	}
	if err := s.events.Add(rows); err != nil {
		return nil, queueErr("cluster_events", err)
	}
	return connect.NewResponse(&kuberov1.IngestEventsResponse{
		Accepted: int32(len(rows)), Dropped: int32(len(in) - len(rows)),
	}), nil
}
