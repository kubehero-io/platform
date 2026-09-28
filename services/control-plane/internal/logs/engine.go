// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package logs is KubeHero's Loki-class log engine: LogQL log and
// metric queries over the ClickHouse log store, log volume with what
// it costs to keep, Drain pattern mining, label discovery and live
// tail. It serves LogsService, backs the Loki-compatible HTTP API
// (internal/compat/loki), and implements signals.LogMetricQuerier for
// kind=logs alert rules.
//
// Without ClickHouse it serves deterministic demo logs (every line
// labelled source="demo") unless KUBEHERO_DEMO_MODE=false, in which
// case every call fails with FailedPrecondition.
package logs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/logschema"
	"github.com/kubehero-io/platform/services/control-plane/internal/patterns"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// Defaults and limits.
const (
	DefaultRange     = time.Hour
	MaxRange         = 31 * 24 * time.Hour
	DefaultLimit     = 100
	MaxLimit         = 5000
	DefaultUSDPerGB  = 0.50
	DefaultPatterns  = 50
	MaxPatterns      = 500
	patternBuckets   = 30
	volumeBuckets    = 60
	maxLabelValues   = 1000
	maxSeriesResults = 1000
)

// Options configures the engine.
type Options struct {
	// Store is the log storage; nil means ClickHouse is not configured.
	Store Store
	// DemoFixturesDisabled refuses to serve demo data without a store.
	DemoFixturesDisabled bool
	// USDPerGB prices ingested log volume (KUBEHERO_LOG_USD_PER_GB).
	USDPerGB float64
	// MaxScanRows caps rows one log query reads to find its lines, and
	// rows Go-evaluated pipelines may process (default 1 000 000).
	MaxScanRows int
	// PatternSample is how many lines GetLogPatterns mines (default 50 000).
	PatternSample int
	// Tail polling: interval (1s), overlap for late lines (5s),
	// lines per poll (5000) and per message (500), maximum duration (1h).
	TailPoll        time.Duration
	TailOverlap     time.Duration
	TailMaxPerPoll  int
	TailMaxPerPush  int
	TailMaxDuration time.Duration
	Now             func() time.Time
	Log             *slog.Logger
}

// Engine is the log query engine.
type Engine struct {
	store Store
	demo  bool
	opts  Options
	now   func() time.Time
	log   *slog.Logger
}

// New builds an engine; see Options for defaults.
func New(o Options) *Engine {
	e := &Engine{store: o.Store, opts: o, now: o.Now, log: o.Log}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	if e.opts.USDPerGB <= 0 {
		e.opts.USDPerGB = DefaultUSDPerGB
	}
	if e.opts.MaxScanRows <= 0 {
		e.opts.MaxScanRows = 1_000_000
	}
	if e.opts.PatternSample <= 0 {
		e.opts.PatternSample = 50_000
	}
	if e.opts.TailPoll <= 0 {
		e.opts.TailPoll = time.Second
	}
	if e.opts.TailOverlap <= 0 {
		e.opts.TailOverlap = 5 * time.Second
	}
	if e.opts.TailMaxPerPoll <= 0 {
		e.opts.TailMaxPerPoll = 5000
	}
	if e.opts.TailMaxPerPush <= 0 {
		e.opts.TailMaxPerPush = 500
	}
	if e.opts.TailMaxDuration <= 0 {
		e.opts.TailMaxDuration = time.Hour
	}
	if e.store == nil && !o.DemoFixturesDisabled {
		e.store = NewDemoStore(e.now)
		e.demo = true
	}
	return e
}

// Demo reports whether the engine serves demo data.
func (e *Engine) Demo() bool { return e.demo }

// Source is "clickhouse", "demo" or "" (unavailable).
func (e *Engine) Source() string {
	if e.store == nil {
		return ""
	}
	return e.store.Source()
}

// ErrUnavailable: no store and demo data disabled.
var ErrUnavailable = errors.New("logs: no ClickHouse configured and demo fixtures are disabled (KUBEHERO_DEMO_MODE=false); set CLICKHOUSE_URL")

// BadRequest is an invalid argument (the message is safe to return).
type BadRequest struct{ Msg string }

func (b *BadRequest) Error() string { return b.Msg }

func badRequest(format string, args ...any) error {
	return &BadRequest{Msg: fmt.Sprintf(format, args...)}
}

func (e *Engine) ready() error {
	if e.store == nil {
		return ErrUnavailable
	}
	return nil
}

// Stats describes the work a query did.
type Stats struct {
	RowsScanned  int64
	BytesScanned int64
	Exec         time.Duration
}

// Line is one log line of a result: the (possibly reformatted) line
// and its labels — stream labels plus labels extracted by parsers.
type Line struct {
	TS      int64
	Body    string
	Labels  map[string]string
	TraceID string
}

// Level is the line's level label ("" when unknown).
func (l Line) Level() string { return l.Labels["level"] }

// QueryParams is a LogQL query request.
type QueryParams struct {
	Query      string
	Start, End time.Time // zero Start = End - 1h, zero End = now
	Limit      int       // log queries; default 100, max 5000
	Forward    bool
	Step       time.Duration // metric queries; 0 = auto
	Instant    bool          // metric queries: evaluate once at End
	ClusterID  string
}

// QueryResult is either Lines (log query) or Metric.
type QueryResult struct {
	Lines  []Line
	Metric *logql.Result
	Step   time.Duration
	Start  time.Time
	End    time.Time
	Stats  Stats
}

// IsMetric reports whether the result is a metric result.
func (r *QueryResult) IsMetric() bool { return r.Metric != nil }

func (e *Engine) timeRange(start, end time.Time, def time.Duration) (time.Time, time.Time, error) {
	if end.IsZero() {
		end = e.now()
	}
	if start.IsZero() {
		start = end.Add(-def)
	}
	if !end.After(start) && !end.Equal(start) {
		return start, end, badRequest("start (%s) must be before end (%s)", start.Format(time.RFC3339), end.Format(time.RFC3339))
	}
	if end.Sub(start) > MaxRange {
		return start, end, badRequest("the time range is longer than %s", logql.FormatDuration(MaxRange))
	}
	return start.UTC(), end.UTC(), nil
}

var clusterRE = func(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
			return false
		}
	}
	return true
}

// scopeCluster adds {cluster="id"} to every selector of a query.
func scopeCluster(expr logql.Expr, cluster string) error {
	if cluster == "" {
		return nil
	}
	if !clusterRE(cluster) {
		return badRequest("cluster_id %q: want 1-128 characters of [A-Za-z0-9._:-]", cluster)
	}
	q := fmt.Sprintf(`{cluster=%q}`, cluster)
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return badRequest("cluster_id: %v", err)
	}
	logql.Walk(expr, func(s *logql.LogSelectorExpr) {
		s.Matchers = append(s.Matchers, sel.Matchers[0])
	})
	return nil
}

var niceSteps = []time.Duration{
	time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second,
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour, 24 * time.Hour,
}

// niceStep picks the smallest "round" step giving at most points
// points over d (round steps keep gcd grids coarse and let minute
// multiples use the rollup).
func niceStep(d time.Duration, points int) time.Duration {
	raw := d / time.Duration(points)
	for _, s := range niceSteps {
		if s >= raw {
			return s
		}
	}
	days := (raw + 24*time.Hour - 1) / (24 * time.Hour)
	return days * 24 * time.Hour
}

// Query runs a LogQL query.
func (e *Engine) Query(ctx context.Context, p QueryParams) (*QueryResult, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	began := time.Now()
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return nil, badRequest("query is required")
	}
	expr, err := logql.Parse(q)
	if err != nil {
		return nil, err
	}
	if err := scopeCluster(expr, p.ClusterID); err != nil {
		return nil, err
	}
	start, end, err := e.timeRange(p.Start, p.End, DefaultRange)
	if err != nil {
		return nil, err
	}
	res := &QueryResult{Start: start, End: end}
	switch x := expr.(type) {
	case *logql.LogSelectorExpr:
		limit := p.Limit
		if limit <= 0 {
			limit = DefaultLimit
		}
		if limit > MaxLimit {
			return nil, badRequest("limit %d exceeds the maximum of %d", limit, MaxLimit)
		}
		lines, st, err := e.selectLines(ctx, x, start.UnixNano(), end.UnixNano(), limit, p.Forward)
		if err != nil {
			return nil, err
		}
		res.Lines, res.Stats = lines, st
	case logql.SampleExpr:
		step := p.Step
		if p.Instant {
			start, step = end, 0
		} else if step <= 0 {
			step = niceStep(end.Sub(start), 250)
		}
		if step < 0 {
			return nil, badRequest("step must be positive")
		}
		m, err := logql.Evaluate(ctx, x, start, end, step, e.store)
		if err != nil {
			return nil, err
		}
		res.Metric, res.Step = m, step
	}
	res.Stats.Exec = time.Since(began)
	return res, nil
}

// selectLines finds up to limit lines, newest first unless forward.
// The range is read in chunks growing from 15 minutes so "the latest
// 100 lines" of a busy week touches only the latest chunk. Pipelines
// SQL cannot express run in Go on each row, bounded by MaxScanRows.
func (e *Engine) selectLines(ctx context.Context, sel *logql.LogSelectorExpr, from, to int64, limit int, forward bool) ([]Line, Stats, error) {
	plan := logql.PlanSelector(sel)
	pipe := logql.NewPipeline(plan.Stages)
	var st Stats
	var out []Line
	budget := e.opts.MaxScanRows
	width := int64(15 * time.Minute)
	lo, hi := from, to
	for len(out) < limit && budget > 0 && lo < hi {
		var cFrom, cTo int64
		if forward {
			cFrom, cTo = lo, min(hi, lo+width)
		} else {
			cFrom, cTo = max(lo, hi-width), hi
		}
		want := budget
		if plan.Exact() {
			want = min(budget, limit-len(out))
		}
		got := 0
		ss, err := e.store.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: cFrom, To: cTo, Forward: forward, Max: want},
			func(r Row) bool {
				got++
				line, labels, ok := pipe.Process(r.TS, r.Body, r.Labels)
				if ok {
					out = append(out, Line{TS: r.TS, Body: line, Labels: logql.SeriesLabels(labels, nil), TraceID: r.TraceID})
				}
				return len(out) < limit
			})
		st.RowsScanned += ss.Rows
		st.BytesScanned += ss.Bytes
		if err != nil {
			return nil, st, err
		}
		budget -= got
		if forward {
			lo = cTo
		} else {
			hi = cFrom
		}
		width *= 2
	}
	return out, st, nil
}

// QueryMetric implements signals.LogMetricQuerier for alert rules.
func (e *Engine) QueryMetric(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]signals.Series, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	expr, err := logql.ParseSampleExpr(query)
	if err != nil {
		return nil, err
	}
	if end.Sub(start) > MaxRange {
		return nil, badRequest("the time range is longer than %s", logql.FormatDuration(MaxRange))
	}
	res, err := logql.Evaluate(ctx, expr, start, end, step, e.store)
	if err != nil {
		return nil, err
	}
	return res.Series, nil
}

var _ signals.LogMetricQuerier = (*Engine)(nil)

// ─── volume ──────────────────────────────────────────────────────────────

// VolumeParams asks for a lines-per-step histogram.
type VolumeParams struct {
	Query      string // selector (+ filters); empty = every stream
	Start, End time.Time
	Step       time.Duration // 0 = auto (~60 buckets)
	GroupBy    string        // default "level"; "-" = no grouping
	ClusterID  string
}

// VolumeResult is a histogram plus totals and cost.
type VolumeResult struct {
	Series          []signals.Series
	TotalLines      int64
	TotalBytes      int64
	EstCostUSDMonth float64
	Step            time.Duration
	Start, End      time.Time
	Stats           Stats
}

// Volume computes a volume histogram. Bare selectors over rollup
// labels read log_volume_1m; anything else scans raw lines.
func (e *Engine) Volume(ctx context.Context, p VolumeParams) (*VolumeResult, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	began := time.Now()
	q := strings.TrimSpace(p.Query)
	if q == "" {
		q = "{}"
	}
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return nil, err
	}
	if err := scopeCluster(sel, p.ClusterID); err != nil {
		return nil, err
	}
	groupBy := p.GroupBy
	switch groupBy {
	case "":
		groupBy = "level"
	case "-":
		groupBy = ""
	}
	if groupBy != "" && !logschema.ValidLabelName(groupBy) {
		return nil, badRequest("group_by %q is not a valid label name", groupBy)
	}
	start, end, err := e.timeRange(p.Start, p.End, DefaultRange)
	if err != nil {
		return nil, err
	}
	step := p.Step
	if step <= 0 {
		step = niceStep(end.Sub(start), volumeBuckets)
	}
	if step < time.Second {
		return nil, badRequest("step must be at least 1s")
	}
	// Align to the step so buckets are stable across refreshes (and,
	// for minute multiples, eligible for the rollup).
	from := start.UnixNano() / int64(step) * int64(step)
	to := (end.UnixNano() + int64(step) - 1) / int64(step) * int64(step)
	if to == from {
		to += int64(step)
	}
	n := int((to - from) / int64(step))
	if n > logql.MaxSteps {
		return nil, badRequest("%d buckets exceed the limit of %d; increase the step", n, logql.MaxSteps)
	}
	plan := logql.PlanSelector(sel)
	cells, st, err := e.store.Volume(ctx, VolumeRequest{
		Selector: sel, Plan: plan, From: from, To: to, Step: int64(step), GroupBy: groupBy, MaxScanned: e.opts.MaxScanRows,
	})
	if err != nil {
		return nil, err
	}
	res := &VolumeResult{Step: step, Start: time.Unix(0, from).UTC(), End: time.Unix(0, to).UTC(),
		Stats: Stats{RowsScanned: st.Rows, BytesScanned: st.Bytes}}
	groups := map[string][]float64{}
	totals := map[string]float64{}
	for _, c := range cells {
		if c.Bucket < 0 || c.Bucket >= n {
			continue
		}
		g := c.Group
		if g == "" && groupBy == "level" {
			g = "unknown"
		}
		if groups[g] == nil {
			groups[g] = make([]float64, n)
		}
		groups[g][c.Bucket] += c.Lines
		totals[g] += c.Lines
		res.TotalLines += int64(c.Lines)
		res.TotalBytes += int64(c.Bytes)
	}
	keys := make([]string, 0, len(groups))
	for g := range groups {
		keys = append(keys, g)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	for _, g := range keys {
		s := signals.Series{Labels: map[string]string{}}
		if groupBy != "" {
			s.Labels[groupBy] = g
		}
		for b, v := range groups[g] {
			s.Points = append(s.Points, signals.Point{TS: time.Unix(0, from+int64(b)*int64(step)).UTC(), Value: v})
		}
		res.Series = append(res.Series, s)
	}
	// $ to keep this volume for a month: bytes per day × 30 at the
	// configured $/GB (decimal GB, how storage and SaaS log tools bill).
	days := end.Sub(start).Hours() / 24
	if days > 0 {
		res.EstCostUSDMonth = float64(res.TotalBytes) / days * 30 / 1e9 * e.opts.USDPerGB
	}
	res.Stats.Exec = time.Since(began)
	return res, nil
}

// ─── patterns ────────────────────────────────────────────────────────────

// PatternParams asks for mined patterns.
type PatternParams struct {
	Query      string
	Start, End time.Time
	Limit      int
	ClusterID  string
}

// PatternResult is the mined patterns.
type PatternResult struct {
	Patterns      []PatternOut
	LinesAnalyzed int64
	Step          time.Duration
	Start         time.Time
}

// PatternOut is one pattern with estimated totals (scaled up when the
// lines were sampled).
type PatternOut struct {
	Pattern  string
	Count    int64
	Level    string
	SharePct float64
	Trend    []signals.Point
	Sample   string
}

// Patterns clusters matching lines with Drain. Busy ranges are sampled
// deterministically down to PatternSample lines; counts and trends are
// scaled back up to estimate the totals.
func (e *Engine) Patterns(ctx context.Context, p PatternParams) (*PatternResult, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(p.Query)
	if q == "" {
		return nil, badRequest("query is required")
	}
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return nil, err
	}
	if err := scopeCluster(sel, p.ClusterID); err != nil {
		return nil, err
	}
	limit := p.Limit
	if limit <= 0 {
		limit = DefaultPatterns
	}
	if limit > MaxPatterns {
		return nil, badRequest("limit %d exceeds the maximum of %d", limit, MaxPatterns)
	}
	start, end, err := e.timeRange(p.Start, p.End, DefaultRange)
	if err != nil {
		return nil, err
	}
	from, to := start.UnixNano(), end.UnixNano()
	if to == from {
		to++
	}
	plan := logql.PlanSelector(sel)
	total, err := e.store.Count(ctx, plan, sel, from, to)
	if err != nil {
		return nil, err
	}
	rate := 1.0
	if total > int64(e.opts.PatternSample) {
		rate = float64(e.opts.PatternSample) / float64(total)
	}
	step := (to - from + patternBuckets - 1) / patternBuckets
	drain := patterns.New(patterns.Config{}, patternBuckets)
	pipe := logql.NewPipeline(plan.Stages)
	_, err = e.store.Scan(ctx, ScanRequest{Selector: sel, Plan: plan, From: from, To: to, Forward: true,
		Max: 2 * e.opts.PatternSample, Sample: rate},
		func(r Row) bool {
			line, labels, ok := pipe.Process(r.TS, r.Body, r.Labels)
			if !ok {
				return true
			}
			drain.Train(line, labels["level"], int((r.TS-from)/step), 1)
			return true
		})
	if err != nil {
		return nil, err
	}
	res := &PatternResult{LinesAnalyzed: drain.Lines(), Step: time.Duration(step), Start: start}
	scale := 1 / rate
	for i, pt := range drain.Patterns() {
		if i >= limit {
			break
		}
		out := PatternOut{
			Pattern: pt.Pattern,
			Count:   int64(math.Round(float64(pt.Count) * scale)),
			Level:   pt.Level,
			Sample:  pt.Sample,
		}
		if n := drain.Lines(); n > 0 {
			out.SharePct = float64(pt.Count) / float64(n) * 100
		}
		for b, c := range pt.Trend {
			out.Trend = append(out.Trend, signals.Point{TS: time.Unix(0, from+int64(b)*step).UTC(), Value: math.Round(float64(c) * scale)})
		}
		res.Patterns = append(res.Patterns, out)
	}
	return res, nil
}

// ─── labels & series ─────────────────────────────────────────────────────

func (e *Engine) labelScope(query, cluster string, start, end time.Time) (*logql.LogSelectorExpr, *logql.Plan, int64, int64, error) {
	q := strings.TrimSpace(query)
	if q == "" {
		q = "{}"
	}
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if err := scopeCluster(sel, cluster); err != nil {
		return nil, nil, 0, 0, err
	}
	s, t, err := e.timeRange(start, end, DefaultRange)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	return sel, logql.PlanSelector(sel), s.UnixNano(), t.UnixNano() + 1, nil
}

// LabelNames lists label names present in the range (optionally
// scoped by a selector).
func (e *Engine) LabelNames(ctx context.Context, query, cluster string, start, end time.Time) ([]string, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	sel, plan, from, to, err := e.labelScope(query, cluster, start, end)
	if err != nil {
		return nil, err
	}
	return e.store.LabelNames(ctx, sel, plan, from, to)
}

// LabelValues lists values of one label.
func (e *Engine) LabelValues(ctx context.Context, name, query, cluster string, start, end time.Time, limit int) ([]string, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if !logschema.ValidLabelName(name) {
		return nil, badRequest("%q is not a valid label name", name)
	}
	if limit <= 0 || limit > maxLabelValues {
		limit = maxLabelValues
	}
	sel, plan, from, to, err := e.labelScope(query, cluster, start, end)
	if err != nil {
		return nil, err
	}
	return e.store.LabelValues(ctx, sel, plan, name, from, to, limit)
}

// Series lists distinct stream label sets for each selector.
func (e *Engine) Series(ctx context.Context, selectors []string, cluster string, start, end time.Time) ([]map[string]string, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		selectors = []string{"{}"}
	}
	seen := map[string]bool{}
	var out []map[string]string
	for _, q := range selectors {
		sel, plan, from, to, err := e.labelScope(q, cluster, start, end)
		if err != nil {
			return nil, err
		}
		got, err := e.store.Series(ctx, sel, plan, from, to, maxSeriesResults)
		if err != nil {
			return nil, err
		}
		for _, l := range got {
			k := logql.LabelsKey(l)
			if !seen[k] && len(out) < maxSeriesResults {
				seen[k] = true
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// GroupTotal is one group's line and byte totals.
type GroupTotal struct {
	Labels map[string]string
	Lines  int64
	Bytes  int64
}

// VolumeTotals returns exact line and byte totals per value of groupBy
// ("" = one total) over exactly [start, end), largest first.
func (e *Engine) VolumeTotals(ctx context.Context, query, cluster, groupBy string, start, end time.Time) ([]GroupTotal, error) {
	if err := e.ready(); err != nil {
		return nil, err
	}
	q := strings.TrimSpace(query)
	if q == "" {
		q = "{}"
	}
	sel, err := logql.ParseLogSelector(q)
	if err != nil {
		return nil, err
	}
	if err := scopeCluster(sel, cluster); err != nil {
		return nil, err
	}
	if groupBy != "" && !logschema.ValidLabelName(groupBy) {
		return nil, badRequest("%q is not a valid label name", groupBy)
	}
	start, end, err = e.timeRange(start, end, DefaultRange)
	if err != nil {
		return nil, err
	}
	from, to := start.UnixNano(), end.UnixNano()
	if to <= from {
		return nil, nil
	}
	cells, _, err := e.store.Volume(ctx, VolumeRequest{
		Selector: sel, Plan: logql.PlanSelector(sel), From: from, To: to, Step: to - from, GroupBy: groupBy, MaxScanned: e.opts.MaxScanRows,
	})
	if err != nil {
		return nil, err
	}
	byGroup := map[string]*GroupTotal{}
	var out []*GroupTotal
	for _, c := range cells {
		g, ok := byGroup[c.Group]
		if !ok {
			labels := map[string]string{}
			if groupBy != "" && c.Group != "" {
				labels[groupBy] = c.Group
			}
			g = &GroupTotal{Labels: labels}
			byGroup[c.Group] = g
			out = append(out, g)
		}
		g.Lines += int64(c.Lines)
		g.Bytes += int64(c.Bytes)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return logql.LabelsKey(out[i].Labels) < logql.LabelsKey(out[j].Labels)
	})
	res := make([]GroupTotal, len(out))
	for i, g := range out {
		res[i] = *g
	}
	return res, nil
}
