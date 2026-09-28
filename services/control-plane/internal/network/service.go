// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package network serves the eBPF service map and per-workload network
// spend from net_flows_1h.
//
// Flows arrive already attributed (endpoint kinds, zones) and priced
// (cross-zone and internet-egress $ from the configured $/GB) at
// ingest, so this package sums and de-duplicates: pod→pod traffic is
// observed at both ends, and each (source, destination) pair is counted
// from exactly one side — the receiver's ingress record when the
// destination is a pod, the sender's egress record otherwise, falling
// back to whichever exists (graph.go). Windows are hour-granular (the
// rollup's resolution); rates divide by the covered hour-aligned span.
package network

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"connectrpc.com/connect"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1/kuberov1connect"
	"github.com/kubehero-io/platform/services/control-plane/internal/auth"
	"github.com/kubehero-io/platform/services/control-plane/internal/chsql"
	"github.com/kubehero-io/platform/services/control-plane/internal/clusters"
	"github.com/kubehero-io/platform/services/control-plane/internal/timewin"
)

// Service implements kuberov1connect.NetworkServiceHandler.
type Service struct {
	CH           *sql.DB
	Clusters     *clusters.Resolver
	DemoDisabled bool
	Now          func() time.Time
	Timeout      time.Duration
	MaxRows      int // flow groups per query, default 200k
}

var _ kuberov1connect.NetworkServiceHandler = (*Service)(nil)

const (
	defaultSpan     = time.Hour
	maxSpan         = 90 * timewin.Day
	defaultMaxNodes = 60
	maxMaxNodes     = 500
)

// ErrTooManyFlows: the window/scope has more distinct flow groups than
// one query may return.
var ErrTooManyFlows = errors.New("too many distinct flows: narrow the time range, cluster or namespace")

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return 30 * time.Second
}

func (s *Service) maxRows() int {
	if s.MaxRows > 0 {
		return s.MaxRows
	}
	return 200_000
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

type scope struct {
	window    timewin.Window
	aliases   []string
	namespace string
}

func (s *Service) parseScope(ctx context.Context, startMS, endMS int64, cluster, namespace string) (scope, error) {
	var sc scope
	w, err := timewin.FromUnixMS(startMS, endMS, defaultSpan, maxSpan, s.now())
	if err != nil {
		return sc, invalid(err)
	}
	if len(namespace) > 63 {
		return sc, invalid(errors.New("namespace too long"))
	}
	snap := s.Clusters.Snapshot(ctx)
	c, err := clusters.Scope(ctx, snap, strings.TrimSpace(cluster))
	if err != nil {
		return sc, err
	}
	return scope{window: w, aliases: snap.Aliases(c), namespace: strings.TrimSpace(namespace)}, nil
}

// span is the hour-aligned interval the rollup actually covers.
func (sc scope) span(now time.Time) (time.Time, time.Time) {
	start, end := timewin.FloorHour(sc.window.Start), timewin.CeilHour(sc.window.QueryEnd())
	if end.After(now) {
		end = now
	}
	return start, end
}

// flows reads the window's flow groups with both observation sides.
func (s *Service) flows(ctx context.Context, sc scope) ([]FlowRow, time.Duration, error) {
	start, end := sc.span(s.now())
	var w chsql.Where
	w.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", start.Unix(), timewin.CeilHour(end).Unix())
	if sc.aliases != nil {
		w.In("cluster_id", sc.aliases)
	}
	if sc.namespace != "" {
		w.Add("(src_namespace = ? OR dst_namespace = ?)", sc.namespace, sc.namespace)
	}
	query := `
		SELECT cluster_id, src_kind, src_namespace, src_workload, src_zone,
		       dst_kind, dst_namespace, if(dst_kind = 'service', dst_service, if(dst_kind = 'pod', dst_workload, dst_name)), dst_zone,
		       toInt32(port), protocol,
		       countIf(direction = 'ingress') > 0, countIf(direction != 'ingress') > 0,
		       sumIf(bytes, direction = 'ingress'), sumIf(bytes, direction != 'ingress'),
		       sumIf(bytes, direction = 'ingress' AND cross_zone = 1), sumIf(bytes, direction != 'ingress' AND cross_zone = 1),
		       sumIf(bytes, direction = 'ingress' AND egress = 1), sumIf(bytes, direction != 'ingress' AND egress = 1),
		       sumIf(cost_usd, direction = 'ingress'), sumIf(cost_usd, direction != 'ingress'),
		       sumIf(cost_usd, direction = 'ingress' AND cross_zone = 1), sumIf(cost_usd, direction != 'ingress' AND cross_zone = 1),
		       sumIf(cost_usd, direction = 'ingress' AND egress = 1), sumIf(cost_usd, direction != 'ingress' AND egress = 1),
		       sumIf(retransmits, direction = 'ingress'), sumIf(retransmits, direction != 'ingress')
		FROM net_flows_1h
		WHERE ` + w.SQL() + `
		GROUP BY cluster_id, src_kind, src_namespace, src_workload, src_zone, dst_kind, dst_namespace,
		         dst_workload, dst_service, dst_name, dst_zone, port, protocol
		LIMIT ?`
	qctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	rows, err := s.CH.QueryContext(qctx, query, append(w.Args(), s.maxRows()+1)...)
	if err != nil {
		return nil, 0, fmt.Errorf("flows query: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	snap := s.Clusters.Snapshot(ctx)
	var out []FlowRow
	for rows.Next() {
		var (
			r                    FlowRow
			cluster              string
			inB, egB, inXB, egXB uint64
			inNB, egNB           uint64
			inR, egR             uint64
		)
		if err := rows.Scan(&cluster, &r.SrcKind, &r.SrcNamespace, &r.SrcName, &r.SrcZone,
			&r.DstKind, &r.DstNamespace, &r.DstName, &r.DstZone, &r.Port, &r.Protocol,
			&r.In.Seen, &r.Eg.Seen, &inB, &egB, &inXB, &egXB, &inNB, &egNB,
			&r.In.Cost, &r.Eg.Cost, &r.In.CrossZoneCost, &r.Eg.CrossZoneCost, &r.In.NetCost, &r.Eg.NetCost,
			&inR, &egR); err != nil {
			return nil, 0, fmt.Errorf("flows scan: %w", err)
		}
		r.Cluster = snap.Display(cluster)
		r.In.Bytes, r.Eg.Bytes = float64(inB), float64(egB)
		r.In.CrossZoneBytes, r.Eg.CrossZoneBytes = float64(inXB), float64(egXB)
		r.In.NetBytes, r.Eg.NetBytes = float64(inNB), float64(egNB)
		r.In.Retransmits, r.Eg.Retransmits = float64(inR), float64(egR)
		out = append(out, r)
		if len(out) > s.maxRows() {
			return nil, 0, ErrTooManyFlows
		}
	}
	return out, end.Sub(start), rows.Err()
}

func toConnectErr(err error) error {
	if errors.Is(err, ErrTooManyFlows) {
		return connect.NewError(connect.CodeResourceExhausted, err)
	}
	return connect.NewError(connect.CodeInternal, err)
}

func (s *Service) demoGate(rpc string) error {
	if s.DemoDisabled {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s: ClickHouse is not configured and demo fixtures are disabled (KUBEHERO_DEMO_MODE=false)", rpc))
	}
	return nil
}

// GetServiceMap returns the workload-level service map.
func (s *Service) GetServiceMap(ctx context.Context, req *connect.Request[kuberov1.GetServiceMapRequest]) (*connect.Response[kuberov1.GetServiceMapResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	sc, err := s.parseScope(ctx, m.GetStartUnixMs(), m.GetEndUnixMs(), m.GetClusterId(), m.GetNamespace())
	if err != nil {
		return nil, err
	}
	maxNodes := int(m.GetMaxNodes())
	if maxNodes <= 0 {
		maxNodes = defaultMaxNodes
	}
	maxNodes = max(2, min(maxNodes, maxMaxNodes))

	var (
		rows   []FlowRow
		span   time.Duration
		source = "live"
	)
	if s.CH == nil {
		if err := s.demoGate("GetServiceMap"); err != nil {
			return nil, err
		}
		rows, span, source = demoFlows(sc.namespace), time.Hour, "demo"
	} else if rows, span, err = s.flows(ctx, sc); err != nil {
		return nil, toConnectErr(err)
	}
	g := Build(rows)
	g.Truncate(maxNodes)
	return connect.NewResponse(mapResponse(g, span, source)), nil
}

func mapResponse(g *Graph, span time.Duration, source string) *kuberov1.GetServiceMapResponse {
	secs := span.Seconds()
	month := func(v float64) float64 { return math.Round(timewin.PerMonth(v, span)*100) / 100 }
	resp := &kuberov1.GetServiceMapResponse{
		TotalCostUsdMonth: month(g.Total.Cost),
		CrossZoneGb:       g.Total.CrossZoneBytes / 1e9,
		EgressGb:          g.Total.NetBytes / 1e9,
		Source:            source,
	}
	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		n := g.Nodes[id]
		resp.Nodes = append(resp.Nodes, &kuberov1.ServiceMapNode{Id: n.ID, Name: n.Name, Namespace: n.Namespace,
			Kind: n.Kind, Zone: n.Zone, BytesIn: n.BytesIn, BytesOut: n.BytesOut, CostUsdMonth: month(n.Cost)})
	}
	for _, e := range g.Edges {
		bps := 0.0
		if secs > 0 {
			bps = e.Bytes / secs
		}
		resp.Edges = append(resp.Edges, &kuberov1.ServiceMapEdge{Source: e.Source, Target: e.Target, Port: e.Port,
			Protocol: e.Protocol, Bytes: e.Bytes, BytesPerSec: bps, CrossZone: e.CrossZoneBytes > 0,
			Egress: e.NetBytes > 0, CostUsdMonth: month(e.Cost), Retransmits: e.Retransmits})
	}
	return resp
}

// ListNetworkCosts returns network spend per source workload.
func (s *Service) ListNetworkCosts(ctx context.Context, req *connect.Request[kuberov1.ListNetworkCostsRequest]) (*connect.Response[kuberov1.ListNetworkCostsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	sc, err := s.parseScope(ctx, m.GetStartUnixMs(), m.GetEndUnixMs(), m.GetClusterId(), "")
	if err != nil {
		return nil, err
	}
	limit := int(m.GetLimit())
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	var (
		rows   []FlowRow
		span   time.Duration
		source = "live"
	)
	if s.CH == nil {
		if err := s.demoGate("ListNetworkCosts"); err != nil {
			return nil, err
		}
		rows, span, source = demoFlows(""), time.Hour, "demo"
	} else if rows, span, err = s.flows(ctx, sc); err != nil {
		return nil, toConnectErr(err)
	}
	costs, total := Costs(Build(rows), span)
	if len(costs) > limit {
		costs = costs[:limit]
	}
	return connect.NewResponse(&kuberov1.ListNetworkCostsResponse{Costs: costs, TotalUsdMonth: total, Source: source}), nil
}

// Costs rolls the graph up per source workload, largest spend first.
func Costs(g *Graph, span time.Duration) ([]*kuberov1.NetworkCost, float64) {
	type acc struct {
		Side
		top    map[string]*Side
		ns, wl string
	}
	by := map[string]*acc{}
	var total float64
	for _, e := range g.Edges {
		src := g.Nodes[e.Source]
		if src == nil || src.Kind != "workload" {
			continue
		}
		a := by[e.Source]
		if a == nil {
			a = &acc{top: map[string]*Side{}, ns: src.Namespace, wl: src.Name}
			by[e.Source] = a
		}
		a.Side.add(e.Side)
		t := a.top[e.Target]
		if t == nil {
			t = &Side{}
			a.top[e.Target] = t
		}
		t.add(e.Side)
		total += e.Cost
	}
	month := func(v float64) float64 { return math.Round(timewin.PerMonth(v, span)*100) / 100 }
	out := make([]*kuberov1.NetworkCost, 0, len(by))
	for _, a := range by {
		var top string
		var best Side
		for id, sd := range a.top {
			if sd.Cost > best.Cost || (sd.Cost == best.Cost && sd.Bytes > best.Bytes) || (sd.Cost == best.Cost && sd.Bytes == best.Bytes && id < top) {
				top, best = id, *sd
			}
		}
		out = append(out, &kuberov1.NetworkCost{
			Namespace:         a.ns,
			Workload:          a.wl,
			EgressGb:          a.NetBytes / 1e9,
			CrossZoneGb:       a.CrossZoneBytes / 1e9,
			EgressUsdMonth:    month(a.NetCost),
			CrossZoneUsdMonth: month(a.CrossZoneCost),
			TotalUsdMonth:     month(a.Cost),
			TopDestination:    displayName(g.Nodes[top]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalUsdMonth != out[j].TotalUsdMonth {
			return out[i].TotalUsdMonth > out[j].TotalUsdMonth
		}
		return out[i].Namespace+"/"+out[i].Workload < out[j].Namespace+"/"+out[j].Workload
	})
	return out, month(total)
}

func displayName(n *Node) string {
	if n == nil {
		return ""
	}
	switch n.Kind {
	case "workload":
		return n.Namespace + "/" + n.Name
	case "service":
		return "svc " + n.Namespace + "/" + n.Name
	case "node":
		return "node " + n.Name
	}
	return n.Name
}
