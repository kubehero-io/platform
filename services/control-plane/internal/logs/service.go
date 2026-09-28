// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package logs

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/logql"
	"github.com/kubehero-io/platform/services/control-plane/internal/signals"
)

// Service adapts the Engine to LogsService.
type Service struct {
	Engine *Engine
}

var _ kuberov1connect.LogsServiceHandler = (*Service)(nil)

// ConnectError maps engine errors onto Connect codes: invalid queries
// are InvalidArgument, expensive ones ResourceExhausted, no store
// FailedPrecondition. Storage failures are logged and returned without
// internals (SQL text never reaches clients).
func (e *Engine) ConnectError(err error) error {
	var pe *logql.ParseError
	var le *logql.Error
	var br *BadRequest
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pe), errors.As(err, &br):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.As(err, &le):
		if le.Kind == logql.KindLimit {
			return connect.NewError(connect.CodeResourceExhausted, err)
		}
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, ErrUnavailable):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("log query timed out; narrow the selector or the time range"))
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	}
	e.log.Error("log query failed", "err", err)
	return connect.NewError(connect.CodeInternal, errors.New("log query failed (see control-plane logs)"))
}

func msTime(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func toProtoSeries(in []signals.Series) []*kuberov1.Series {
	out := make([]*kuberov1.Series, 0, len(in))
	for _, s := range in {
		ps := &kuberov1.Series{Labels: s.Labels, Points: make([]*kuberov1.Point, 0, len(s.Points))}
		for _, p := range s.Points {
			ps.Points = append(ps.Points, &kuberov1.Point{TsUnixMs: p.TS.UnixMilli(), Value: p.Value})
		}
		out = append(out, ps)
	}
	return out
}

func toProtoLine(l Line) *kuberov1.LogLine {
	return &kuberov1.LogLine{TsUnixNano: l.TS, Body: l.Body, Level: l.Level(), Labels: l.Labels, TraceId: l.TraceID}
}

// QueryLogs runs a LogQL log or metric query.
func (s *Service) QueryLogs(ctx context.Context, req *connect.Request[kuberov1.QueryLogsRequest]) (*connect.Response[kuberov1.QueryLogsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	dir := m.GetDirection()
	if dir != "" && dir != "backward" && dir != "forward" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New(`direction must be "backward" or "forward"`))
	}
	if m.GetStepMs() < 0 || m.GetLimit() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("limit and step_ms must not be negative"))
	}
	res, err := s.Engine.Query(ctx, QueryParams{
		Query: m.GetQuery(), Start: msTime(m.GetStartUnixMs()), End: msTime(m.GetEndUnixMs()),
		Limit: int(m.GetLimit()), Forward: dir == "forward", Step: time.Duration(m.GetStepMs()) * time.Millisecond,
		ClusterID: m.GetClusterId(),
	})
	if err != nil {
		return nil, s.Engine.ConnectError(err)
	}
	out := &kuberov1.QueryLogsResponse{Stats: &kuberov1.QueryStats{
		RowsScanned: res.Stats.RowsScanned, BytesScanned: res.Stats.BytesScanned,
		ExecMs: float64(res.Stats.Exec.Microseconds()) / 1000,
	}}
	if res.IsMetric() {
		out.ResultType = "matrix"
		out.Series = toProtoSeries(res.Metric.Series)
	} else {
		out.ResultType = "streams"
		out.Lines = make([]*kuberov1.LogLine, 0, len(res.Lines))
		for _, l := range res.Lines {
			out.Lines = append(out.Lines, toProtoLine(l))
		}
	}
	return connect.NewResponse(out), nil
}

// GetLogVolume returns a lines-per-step histogram and ingest cost.
func (s *Service) GetLogVolume(ctx context.Context, req *connect.Request[kuberov1.GetLogVolumeRequest]) (*connect.Response[kuberov1.GetLogVolumeResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetStepMs() < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("step_ms must not be negative"))
	}
	res, err := s.Engine.Volume(ctx, VolumeParams{
		Query: m.GetQuery(), Start: msTime(m.GetStartUnixMs()), End: msTime(m.GetEndUnixMs()),
		Step: time.Duration(m.GetStepMs()) * time.Millisecond, GroupBy: m.GetGroupBy(), ClusterID: m.GetClusterId(),
	})
	if err != nil {
		return nil, s.Engine.ConnectError(err)
	}
	return connect.NewResponse(&kuberov1.GetLogVolumeResponse{
		Series: toProtoSeries(res.Series), TotalLines: res.TotalLines, TotalBytes: res.TotalBytes,
		EstCostUsdMonth: res.EstCostUSDMonth,
	}), nil
}

// GetLogPatterns clusters lines into Drain templates.
func (s *Service) GetLogPatterns(ctx context.Context, req *connect.Request[kuberov1.GetLogPatternsRequest]) (*connect.Response[kuberov1.GetLogPatternsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	res, err := s.Engine.Patterns(ctx, PatternParams{
		Query: m.GetQuery(), Start: msTime(m.GetStartUnixMs()), End: msTime(m.GetEndUnixMs()),
		Limit: int(m.GetLimit()), ClusterID: m.GetClusterId(),
	})
	if err != nil {
		return nil, s.Engine.ConnectError(err)
	}
	out := &kuberov1.GetLogPatternsResponse{LinesAnalyzed: res.LinesAnalyzed}
	for _, p := range res.Patterns {
		lp := &kuberov1.LogPattern{Pattern: p.Pattern, Count: p.Count, Level: p.Level, SharePct: p.SharePct, Sample: p.Sample}
		for _, pt := range p.Trend {
			lp.Trend = append(lp.Trend, &kuberov1.Point{TsUnixMs: pt.TS.UnixMilli(), Value: pt.Value})
		}
		out.Patterns = append(out.Patterns, lp)
	}
	return connect.NewResponse(out), nil
}

// ListLogLabels returns label names, or the values of one label.
func (s *Service) ListLogLabels(ctx context.Context, req *connect.Request[kuberov1.ListLogLabelsRequest]) (*connect.Response[kuberov1.ListLogLabelsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	start, end := msTime(m.GetStartUnixMs()), msTime(m.GetEndUnixMs())
	if m.GetName() == "" {
		names, err := s.Engine.LabelNames(ctx, m.GetQuery(), m.GetClusterId(), start, end)
		if err != nil {
			return nil, s.Engine.ConnectError(err)
		}
		return connect.NewResponse(&kuberov1.ListLogLabelsResponse{Names: names}), nil
	}
	vals, err := s.Engine.LabelValues(ctx, m.GetName(), m.GetQuery(), m.GetClusterId(), start, end, 0)
	if err != nil {
		return nil, s.Engine.ConnectError(err)
	}
	return connect.NewResponse(&kuberov1.ListLogLabelsResponse{Values: vals}), nil
}

// TailLogs streams new matching lines.
func (s *Service) TailLogs(ctx context.Context, req *connect.Request[kuberov1.TailLogsRequest], stream *connect.ServerStream[kuberov1.TailLogsResponse]) error {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return err
	}
	m := req.Msg
	err := s.Engine.Tail(ctx, TailParams{Query: m.GetQuery(), Start: msTime(m.GetStartUnixMs()), ClusterID: m.GetClusterId()},
		func(b TailBatch) error {
			out := &kuberov1.TailLogsResponse{Dropped: b.Dropped, Lines: make([]*kuberov1.LogLine, 0, len(b.Lines))}
			for _, l := range b.Lines {
				out.Lines = append(out.Lines, toProtoLine(l))
			}
			return stream.Send(out)
		})
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce
		}
		return s.Engine.ConnectError(err)
	}
	return nil
}
