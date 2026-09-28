// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package cost is KubeHero's allocation engine: OpenCost-class cost
// allocation by any dimension (idle and shared-namespace handling,
// efficiency, network and log attribution), spend time series with a
// month-end forecast, and the efficiency score. It also serves the
// CostService Connect API; the OpenCost-compatible and FOCUS HTTP
// faces (internal/compat/opencost, internal/focus) reuse the engine.
//
// All money is USD. "Per month" always means a 30-day month
// (timewin.MonthHours), matching the burn-rate and anomaly code.
package cost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/rightsizing"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Allocator is implemented by the live Engine and the demo fleet.
type Allocator interface {
	Allocate(ctx context.Context, q AllocationQuery) ([]AllocSet, error)
}

// Sources reported to clients.
const (
	SourceLive = "live"
	SourceDemo = "demo"
)

// Service implements kuberov1connect.CostServiceHandler.
type Service struct {
	Engine       *Engine             // nil when ClickHouse is unconfigured
	Rightsizing  *rightsizing.Engine // nil when ClickHouse is unconfigured
	Clusters     *clusters.Resolver
	DemoDisabled bool
	Log          *slog.Logger
	Now          func() time.Time
}

var _ kuberov1connect.CostServiceHandler = (*Service)(nil)

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// allocator picks live or demo data; the error is FailedPrecondition
// when neither is allowed.
func (s *Service) allocator(rpc string) (Allocator, string, error) {
	if s.Engine != nil && s.Engine.CH != nil {
		return s.Engine, SourceLive, nil
	}
	if s.DemoDisabled {
		return nil, "", ErrDemoDisabled(rpc)
	}
	return demoAllocator{}, SourceDemo, nil
}

// ErrDemoDisabled is the FailedPrecondition every read returns when
// ClickHouse is missing and KUBEHERO_DEMO_MODE=false.
func ErrDemoDisabled(rpc string) *connect.Error {
	return connect.NewError(connect.CodeFailedPrecondition,
		fmt.Errorf("%s: ClickHouse is not configured and demo fixtures are disabled (KUBEHERO_DEMO_MODE=false); set CLICKHOUSE_URL", rpc))
}

// Allocator exposes the live-or-demo choice to the HTTP faces.
func (s *Service) Allocator(rpc string) (Allocator, string, error) { return s.allocator(rpc) }

// errCode maps engine errors to Connect codes.
func errCode(err error) error {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, ErrTooManyRows), errors.Is(err, rightsizing.ErrTooManyContainers):
		return connect.NewError(connect.CodeResourceExhausted, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

// splitFilterMap turns proto filters (dim → "a,b") into value lists.
func splitFilterMap(in map[string]string) map[string][]string {
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = strings.Split(v, ",")
	}
	return out
}

// ─── GetAllocation ───────────────────────────────────────────────────

// ParseAllocationRequest validates a GetAllocation request.
func (s *Service) ParseAllocationRequest(ctx context.Context, m *kuberov1.GetAllocationRequest) (AllocationQuery, error) {
	var q AllocationQuery
	w, err := timewin.ParseDefault(m.GetWindow(), "7d", s.now())
	if err != nil {
		return q, invalid(err)
	}
	dims, err := ParseAggregate(m.GetAggregate())
	if err != nil {
		return q, invalid(err)
	}
	filters, err := ParseFilters(splitFilterMap(m.GetFilters()))
	if err != nil {
		return q, invalid(err)
	}
	shareIdle, err := ParseShareIdle(m.GetShareIdle())
	if err != nil {
		return q, invalid(err)
	}
	shared, err := ParseSharedNamespaces(m.GetSharedNamespaces())
	if err != nil {
		return q, invalid(err)
	}
	cluster, err := clusters.Scope(ctx, s.Clusters.Snapshot(ctx), strings.TrimSpace(m.GetClusterId()))
	if err != nil {
		return q, err
	}
	return AllocationQuery{Window: w, Dims: dims, Filters: filters, ClusterID: cluster,
		IncludeIdle: m.GetIncludeIdle(), ShareIdle: shareIdle, SharedNamespaces: shared}, nil
}

// GetAllocation is OpenCost-class allocation over one window.
func (s *Service) GetAllocation(ctx context.Context, req *connect.Request[kuberov1.GetAllocationRequest]) (*connect.Response[kuberov1.GetAllocationResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	q, err := s.ParseAllocationRequest(ctx, req.Msg)
	if err != nil {
		return nil, err
	}
	alloc, source, err := s.allocator("GetAllocation")
	if err != nil {
		return nil, err
	}
	sets, err := alloc.Allocate(ctx, q)
	if err != nil {
		return nil, errCode(err)
	}
	resp := &kuberov1.GetAllocationResponse{
		StartUnixMs: q.Window.Start.UnixMilli(),
		EndUnixMs:   q.Window.QueryEnd().UnixMilli(),
		Source:      source,
	}
	if len(sets) > 0 {
		for _, a := range sets[0].Rows {
			resp.Allocations = append(resp.Allocations, AllocToProto(a))
		}
		resp.Totals = AllocToProto(sets[0].Total)
	}
	return connect.NewResponse(resp), nil
}

// AllocToProto renders one allocation.
func AllocToProto(a *Alloc) *kuberov1.Allocation {
	minutes := a.Minutes()
	perSecond := func(v float64) float64 {
		if minutes <= 0 {
			return 0
		}
		return v / (minutes * 60)
	}
	props := make(map[string]string, len(a.Props))
	for k, v := range a.Props {
		props[k] = v
	}
	return &kuberov1.Allocation{
		Name:                  a.Name,
		Properties:            props,
		StartUnixMs:           a.Start.UnixMilli(),
		EndUnixMs:             a.End.UnixMilli(),
		Minutes:               minutes,
		CpuCoreHours:          a.CPUAllocCS / 3600,
		CpuCoreRequestAverage: perSecond(a.CPUReqCS),
		CpuCoreUsageAverage:   perSecond(a.CPUUseCS),
		CpuCost:               a.CPUCost,
		CpuEfficiency:         a.CPUEfficiency(),
		RamByteHours:          a.RAMAllocBS / 3600,
		RamByteRequestAverage: perSecond(a.RAMReqBS),
		RamByteUsageAverage:   perSecond(a.RAMUseBS),
		RamCost:               a.RAMCost,
		RamEfficiency:         a.RAMEfficiency(),
		GpuHours:              a.GPUSec / 3600,
		GpuCost:               a.GPUCost,
		NetworkCost:           a.NetCost,
		PvCost:                0, // persistent volumes are not collected yet
		SharedCost:            a.SharedCost,
		IdleCost:              a.IdleCost,
		TotalCost:             a.TotalCost(),
		TotalEfficiency:       a.TotalEfficiency(),
		RecoverableCost:       a.Recoverable,
		LogIngestGb:           a.LogBytes / 1e9,
	}
}

// ─── GetCostTimeseries ───────────────────────────────────────────────

var timeseriesGroups = map[string]bool{
	"": true, DimNamespace: true, DimTeam: true, DimCluster: true, DimNodepool: true, DimWorkload: true,
	DimCostCenter: true, DimZone: true, DimCloud: true, DimRegion: true, DimLifecycle: true, DimController: true,
}

const (
	defaultTop = 10
	maxTop     = 50
	otherName  = "other"
)

// GetCostTimeseries returns allocated spend (compute + network; idle
// capacity excluded — see GetAllocation include_idle) per step,
// optionally grouped with top-N + "other", and a month-end forecast.
func (s *Service) GetCostTimeseries(ctx context.Context, req *connect.Request[kuberov1.GetCostTimeseriesRequest]) (*connect.Response[kuberov1.GetCostTimeseriesResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	now := s.now()
	w, err := timewin.ParseDefault(m.GetWindow(), "30d", now)
	if err != nil {
		return nil, invalid(err)
	}
	var step time.Duration
	switch st := strings.TrimSpace(m.GetStep()); st {
	case "":
		step = timewin.Day
		if w.Covered() <= 48*time.Hour {
			step = time.Hour
		}
	default:
		if step, err = timewin.ParseStep(st); err != nil {
			return nil, invalid(err)
		}
	}
	if step == timewin.Day {
		// Daily steps sit on midnight UTC.
		w.Start = w.Start.Truncate(timewin.Day)
	}
	group := strings.ToLower(strings.TrimSpace(m.GetGroupBy()))
	if !timeseriesGroups[group] {
		return nil, invalid(fmt.Errorf("group_by %q not supported", m.GetGroupBy()))
	}
	top := int(m.GetTop())
	if top <= 0 {
		top = defaultTop
	}
	if top > maxTop {
		top = maxTop
	}
	filters, err := ParseFilters(splitFilterMap(m.GetFilters()))
	if err != nil {
		return nil, invalid(err)
	}
	cluster, err := clusters.Scope(ctx, s.Clusters.Snapshot(ctx), strings.TrimSpace(m.GetClusterId()))
	if err != nil {
		return nil, err
	}
	alloc, source, err := s.allocator("GetCostTimeseries")
	if err != nil {
		return nil, err
	}
	dims := []Dim{{Name: DimCluster}}
	if group != "" {
		dims = []Dim{{Name: group}}
	}
	q := AllocationQuery{Window: w, Dims: dims, Filters: filters, ClusterID: cluster, Step: step}
	sets, err := alloc.Allocate(ctx, q)
	if err != nil {
		return nil, errCode(err)
	}
	series, total := buildSeries(sets, group, top)

	forecast, err := s.forecast(ctx, alloc, filters, cluster, now)
	if err != nil {
		return nil, errCode(err)
	}
	return connect.NewResponse(&kuberov1.GetCostTimeseriesResponse{
		Series:           series,
		TotalUsd:         total,
		ForecastMonthUsd: forecast,
		Source:           source,
	}), nil
}

// buildSeries turns stepped allocation sets into dense series with the
// top N groups (by window total) and the rest folded into "other".
func buildSeries(sets []AllocSet, group string, top int) ([]*kuberov1.Series, float64) {
	byName := map[string][]float64{}
	var order []string
	var total float64
	for i, set := range sets {
		for _, a := range set.Rows {
			name := "total"
			if group != "" {
				name = a.Name
			}
			v := a.TotalCost()
			total += v
			pts, ok := byName[name]
			if !ok {
				pts = make([]float64, len(sets))
				byName[name] = pts
				order = append(order, name)
			}
			pts[i] += v
		}
	}
	sum := func(p []float64) (s float64) {
		for _, v := range p {
			s += v
		}
		return s
	}
	sort.SliceStable(order, func(i, j int) bool { return sum(byName[order[i]]) > sum(byName[order[j]]) })
	if group != "" && len(order) > top {
		other := make([]float64, len(sets))
		for _, n := range order[top:] {
			for i, v := range byName[n] {
				other[i] += v
			}
			delete(byName, n)
		}
		order = append(order[:top], otherName)
		byName[otherName] = other
	}
	if len(order) == 0 {
		order = []string{"total"}
		byName["total"] = make([]float64, len(sets))
	}
	out := make([]*kuberov1.Series, 0, len(order))
	for _, n := range order {
		labels := map[string]string{"name": n}
		if group != "" {
			labels[group] = n
		}
		s := &kuberov1.Series{Labels: labels}
		for i, v := range byName[n] {
			s.Points = append(s.Points, &kuberov1.Point{TsUnixMs: sets[i].Start.UnixMilli(), Value: v})
		}
		out = append(out, s)
	}
	return out, total
}

// forecast projects the current month: month-to-date spend plus the
// trailing 7-day daily average for each remaining day.
func (s *Service) forecast(ctx context.Context, alloc Allocator, filters []Filter, cluster string, now time.Time) (float64, error) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	monthEnd := monthStart.AddDate(0, 1, 0)
	total := func(start, end time.Time) (float64, error) {
		if !end.After(start) {
			return 0, nil
		}
		sets, err := alloc.Allocate(ctx, AllocationQuery{
			Window:  timewin.Window{Start: start, End: end, Now: now},
			Dims:    []Dim{{Name: DimCluster}},
			Filters: filters, ClusterID: cluster,
		})
		if err != nil || len(sets) == 0 {
			return 0, err
		}
		return sets[0].Total.TotalCost(), nil
	}
	mtd, err := total(monthStart, now)
	if err != nil {
		return 0, err
	}
	trailingEnd := timewin.FloorHour(now)
	last7, err := total(trailingEnd.Add(-7*timewin.Day), trailingEnd)
	if err != nil {
		return 0, err
	}
	remainingDays := monthEnd.Sub(now).Hours() / 24
	return mtd + last7/7*remainingDays, nil
}

// ─── ListRightsizing ─────────────────────────────────────────────────

// RightsizingSource returns live or demo recommendations for a query.
func (s *Service) RightsizingSource(ctx context.Context, q rightsizing.Query, rpc string) ([]rightsizing.Recommendation, string, error) {
	if s.Rightsizing != nil && s.Rightsizing.CH != nil {
		recs, err := s.Rightsizing.Recommend(ctx, q)
		return recs, SourceLive, err
	}
	if s.DemoDisabled {
		return nil, "", ErrDemoDisabled(rpc)
	}
	var out []rightsizing.Recommendation
	for _, r := range rightsizing.Demo(q.Options) {
		if (q.ClusterID == "" || r.Cluster == q.ClusterID) && (q.Namespace == "" || r.Namespace == q.Namespace) &&
			(q.Workload == "" || r.Workload == q.Workload) {
			out = append(out, r)
		}
	}
	return out, SourceDemo, nil
}

// ListRightsizing returns percentile-based request recommendations.
func (s *Service) ListRightsizing(ctx context.Context, req *connect.Request[kuberov1.ListRightsizingRequest]) (*connect.Response[kuberov1.ListRightsizingResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	w, err := timewin.ParseDefault(m.GetWindow(), "7d", s.now())
	if err != nil {
		return nil, invalid(err)
	}
	if w.Duration() > 90*timewin.Day {
		return nil, invalid(errors.New("window longer than 90d"))
	}
	headroom := m.GetHeadroomPct()
	if headroom < 0 || headroom > 200 || math.IsNaN(headroom) {
		return nil, invalid(errors.New("headroom_pct must be within 0..200"))
	}
	pct, err := rightsizing.ValidPercentile(m.GetCpuPercentile())
	if err != nil {
		return nil, invalid(err)
	}
	minSavings := m.GetMinSavingsUsdMonth()
	if minSavings < 0 || math.IsNaN(minSavings) {
		return nil, invalid(errors.New("min_savings_usd_month must be ≥ 0"))
	}
	limit := int(m.GetLimit())
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	if len(m.GetNamespace()) > 63 {
		return nil, invalid(errors.New("namespace too long"))
	}
	cluster, err := clusters.Scope(ctx, s.Clusters.Snapshot(ctx), strings.TrimSpace(m.GetClusterId()))
	if err != nil {
		return nil, err
	}
	label := strings.TrimSpace(m.GetWindow())
	if label == "" {
		label = "7d"
	}
	recs, source, err := s.RightsizingSource(ctx, rightsizing.Query{
		ClusterID: cluster, Namespace: strings.TrimSpace(m.GetNamespace()), Window: w,
		Options: rightsizing.Options{HeadroomPct: headroom, CPUPercentile: pct, WindowLabel: label},
	}, "ListRightsizing")
	if err != nil {
		return nil, errCode(err)
	}
	kept, total := rightsizing.Filter(recs, minSavings, limit)
	resp := &kuberov1.ListRightsizingResponse{TotalSavingsUsdMonth: total, Source: source}
	for _, r := range kept {
		resp.Recommendations = append(resp.Recommendations, RecToProto(r))
	}
	return connect.NewResponse(resp), nil
}

// RecToProto renders one recommendation.
func RecToProto(r rightsizing.Recommendation) *kuberov1.RightsizingRecommendation {
	return &kuberov1.RightsizingRecommendation{
		Id: r.ID, Cluster: r.Cluster, Namespace: r.Namespace, Workload: r.Workload,
		WorkloadKind: r.WorkloadKind, Container: r.Container, Replicas: int32(math.Round(r.Replicas)),
		CpuRequestCores: r.CPURequest, CpuLimitCores: r.CPULimit,
		CpuP50Cores: r.CPUP50, CpuP95Cores: r.CPUP95, CpuP99Cores: r.CPUP99, CpuMaxCores: r.CPUMax,
		CpuRecommendedCores: r.CPURecommended,
		MemRequestBytes:     u64(r.MemRequest), MemLimitBytes: u64(r.MemLimit),
		MemP50Bytes: u64(r.MemP50), MemP99Bytes: u64(r.MemP99), MemMaxBytes: u64(r.MemMax),
		MemRecommendedBytes:     u64(r.MemRecommended),
		CurrentCostUsdMonth:     r.CurrentCostMonth,
		RecommendedCostUsdMonth: r.RecommendedCost,
		SavingsUsdMonth:         r.SavingsMonth,
		Samples:                 r.Samples, Window: r.Window, Confidence: r.Confidence,
		Direction: r.Direction, Reason: r.Reason, OomKills: int32(r.OOMKills), CpuThrottleRisk: r.ThrottleRisk,
	}
}

func u64(v float64) uint64 {
	if v <= 0 || math.IsNaN(v) {
		return 0
	}
	return uint64(math.Round(v))
}

// ─── GetEfficiency ───────────────────────────────────────────────────

// Score is the efficiency score of one scope, 0..100:
//
//	blend = (min(cpuEff,1)·cpu$ + min(ramEff,1)·ram$) / (cpu$ + ram$)
//	score = 100 · blend · (1 − idle$ / (allocated$ + idle$))
//
// i.e. the share of every compute dollar — including the dollars paid
// for idle node capacity — that does measured work. Utilisation above
// the request is capped at 1: it is risk, not extra efficiency.
// Namespaces carry no idle, so their score is the blend alone.
func Score(m *metrics, idle float64) float64 {
	cpuE, ramE := math.Min(m.CPUEfficiency(), 1), math.Min(m.RAMEfficiency(), 1)
	var blend float64
	if d := m.CPUCost + m.RAMCost; d > 0 {
		blend = (cpuE*m.CPUCost + ramE*m.RAMCost) / d
	}
	idleShare := 0.0
	if tot := m.Cost + idle; tot > 0 {
		idleShare = idle / tot
	}
	return clamp(100*blend*(1-idleShare), 0, 100)
}

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

// GetEfficiency scores the fleet, each cluster and each namespace.
func (s *Service) GetEfficiency(ctx context.Context, req *connect.Request[kuberov1.GetEfficiencyRequest]) (*connect.Response[kuberov1.GetEfficiencyResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	w, err := timewin.ParseDefault(req.Msg.GetWindow(), "7d", s.now())
	if err != nil {
		return nil, invalid(err)
	}
	cluster, err := clusters.Scope(ctx, s.Clusters.Snapshot(ctx), strings.TrimSpace(req.Msg.GetClusterId()))
	if err != nil {
		return nil, err
	}
	alloc, source, err := s.allocator("GetEfficiency")
	if err != nil {
		return nil, err
	}
	sets, err := alloc.Allocate(ctx, AllocationQuery{Window: w, ClusterID: cluster,
		Dims: []Dim{{Name: DimCluster}, {Name: DimNamespace}}, IncludeIdle: true})
	if err != nil {
		return nil, errCode(err)
	}
	covered := w.Covered()
	type acc struct {
		m    metrics
		idle float64
	}
	var fleet acc
	byCluster := map[string]*acc{}
	byNS := map[string]*acc{}
	get := func(mp map[string]*acc, k string) *acc {
		a := mp[k]
		if a == nil {
			a = &acc{}
			mp[k] = a
		}
		return a
	}
	if len(sets) > 0 {
		for _, a := range sets[0].Rows {
			c := a.Key[0]
			if a.Idle {
				fleet.idle += a.IdleCost
				get(byCluster, c).idle += a.IdleCost
				continue
			}
			fleet.m.add(&a.metrics)
			get(byCluster, c).m.add(&a.metrics)
			get(byNS, a.Key[1]).m.add(&a.metrics)
		}
	}
	breakdown := func(mp map[string]*acc) []*kuberov1.EfficiencyBreakdown {
		out := make([]*kuberov1.EfficiencyBreakdown, 0, len(mp))
		for name, a := range mp {
			out = append(out, &kuberov1.EfficiencyBreakdown{
				Name:              displayValue(name),
				CpuEfficiency:     a.m.CPUEfficiency(),
				RamEfficiency:     a.m.RAMEfficiency(),
				IdleCostUsdMonth:  timewin.PerMonth(a.idle, covered),
				TotalCostUsdMonth: timewin.PerMonth(a.m.Cost+a.idle, covered),
				Score:             Score(&a.m, a.idle),
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].TotalCostUsdMonth > out[j].TotalCostUsdMonth })
		return out
	}

	// Recoverable comes from the rightsizing engine, over the same
	// window (at least a day, so percentiles mean something).
	rw := w
	if rw.Covered() < timewin.Day {
		rw.Start = rw.QueryEnd().Add(-timewin.Day)
	}
	var recoverable float64
	if recs, _, err := s.RightsizingSource(ctx, rightsizing.Query{ClusterID: cluster, Window: rw}, "GetEfficiency"); err == nil {
		for _, r := range recs {
			if r.SavingsMonth > 0 && r.Direction == rightsizing.Downsize {
				recoverable += r.SavingsMonth
			}
		}
	} else if s.Log != nil {
		s.Log.Warn("efficiency: rightsizing unavailable", "err", err)
	}

	return connect.NewResponse(&kuberov1.GetEfficiencyResponse{
		Score:               Score(&fleet.m, fleet.idle),
		CpuEfficiency:       fleet.m.CPUEfficiency(),
		RamEfficiency:       fleet.m.RAMEfficiency(),
		IdleCostUsdMonth:    timewin.PerMonth(fleet.idle, covered),
		RecoverableUsdMonth: recoverable,
		Clusters:            breakdown(byCluster),
		Namespaces:          breakdown(byNS),
		Source:              source,
	}), nil
}
