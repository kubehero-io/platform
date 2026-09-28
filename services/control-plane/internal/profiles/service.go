// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package profiles serves continuous profiling: which services have
// profiles, merged (and diff) flamegraphs, and top functions — priced,
// because a CPU profile of a workload is a breakdown of its CPU bill.
//
// Storage: profile_samples holds (service, type, stack_hash, value)
// rows; profile_stacks maps content-addressed stack hashes to frames.
// A flamegraph aggregates SUM(value) per stack for the selector in
// ClickHouse (top 50k stacks by value; the long tail is kept as one
// "[other stacks]" frame so totals stay exact), fetches frames by hash
// and merges the call tree in Go (tree.go).
//
// Pricing: cost_usd_month for cpu profiles is the workload's CPU $ from
// workload_cost_1h over the window, scaled to a month; a frame costs
// cost_usd_month × frame.total / root.total. That attributes the whole
// CPU bill — including requested-but-idle cores — to code paths in
// proportion to where CPU time went.
package profiles

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"regexp"
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

// Service implements kuberov1connect.ProfilesServiceHandler.
type Service struct {
	CH           *sql.DB // nil → demo fixtures (unless DemoDisabled)
	Clusters     *clusters.Resolver
	DemoDisabled bool
	Now          func() time.Time
	Timeout      time.Duration
	MaxStacks    int // default 50k
}

var _ kuberov1connect.ProfilesServiceHandler = (*Service)(nil)

const (
	defaultSpan     = time.Hour
	maxSpan         = 14 * timewin.Day // profile TTL
	defaultMaxNodes = 2048
	maxMaxNodes     = 20000
	frameChunk      = 4000 // hashes per frames query (max_query_size)
	otherStacks     = "[other stacks]"
	unknownStack    = "[unknown stack]"
)

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

func (s *Service) maxStacks() int {
	if s.MaxStacks > 0 {
		return s.MaxStacks
	}
	return 50_000
}

func (s *Service) live() bool { return s.CH != nil }

func (s *Service) demoGate(rpc string) error {
	if s.DemoDisabled {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("%s: ClickHouse is not configured and demo fixtures are disabled (KUBEHERO_DEMO_MODE=false)", rpc))
	}
	return nil
}

func invalid(err error) error { return connect.NewError(connect.CodeInvalidArgument, err) }

var (
	typeRE     = regexp.MustCompile(`^[a-z_]{1,32}$`)
	labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.\-/]{1,128}$`)
)

// selector is a validated ProfileSelector.
type selector struct {
	service, typ, namespace, pod string
	clusterAliases               []string
	labels                       map[string]string
}

func (s *Service) parseSelector(ctx context.Context, sel *kuberov1.ProfileSelector) (selector, error) {
	var out selector
	if sel == nil || strings.TrimSpace(sel.GetService()) == "" {
		return out, invalid(errors.New("selector.service is required"))
	}
	out.service = strings.TrimSpace(sel.GetService())
	out.typ = strings.TrimSpace(sel.GetType())
	if out.typ == "" {
		out.typ = "cpu"
	}
	if len(out.service) > 253 || !typeRE.MatchString(out.typ) {
		return out, invalid(errors.New("invalid service or profile type"))
	}
	out.namespace, out.pod = strings.TrimSpace(sel.GetNamespace()), strings.TrimSpace(sel.GetPod())
	if len(out.namespace) > 63 || len(out.pod) > 253 {
		return out, invalid(errors.New("namespace or pod too long"))
	}
	if len(sel.GetLabels()) > 10 {
		return out, invalid(errors.New("at most 10 label matchers"))
	}
	for k, v := range sel.GetLabels() {
		if !labelKeyRE.MatchString(k) || len(v) > 256 {
			return out, invalid(fmt.Errorf("invalid label matcher %q", k))
		}
	}
	out.labels = sel.GetLabels()
	snap := s.Clusters.Snapshot(ctx)
	cluster, err := clusters.Scope(ctx, snap, strings.TrimSpace(sel.GetClusterId()))
	if err != nil {
		return out, err
	}
	out.clusterAliases = snap.Aliases(cluster)
	return out, nil
}

func (sel selector) where(w *chsql.Where, start, end time.Time) {
	w.Add("service = ? AND type = ?", sel.service, sel.typ)
	w.Add("ts >= toDateTime(?) AND ts < toDateTime(?)", start.Unix(), end.Unix())
	if sel.clusterAliases != nil {
		w.In("cluster_id", sel.clusterAliases)
	}
	if sel.namespace != "" {
		w.Add("namespace = ?", sel.namespace)
	}
	if sel.pod != "" {
		w.Add("pod = ?", sel.pod)
	}
	keys := make([]string, 0, len(sel.labels))
	for k := range sel.labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w.Add("labels[?] = ?", k, sel.labels[k])
	}
}

// profileData is one window's stacks for a selector.
type profileData struct {
	stacks   []Stack
	total    int64 // Σ value over every sample, not just the top stacks
	samples  int64
	unit     string
	workload []wl // distinct workloads profiled (for pricing)
}

type wl struct{ cluster, namespace, workload string }

// load fetches the top stacks and their frames for one window.
func (s *Service) load(ctx context.Context, sel selector, start, end time.Time) (*profileData, error) {
	var w chsql.Where
	sel.where(&w, start, end)
	qctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	d := &profileData{}
	var wls [][]string
	if err := s.CH.QueryRowContext(qctx, `
		SELECT sum(value), count(), any(unit),
		       arraySlice(groupUniqArray(1000)([cluster_id, namespace, workload]), 1, 1000)
		FROM profile_samples WHERE `+w.SQL(), w.Args()...).Scan(&d.total, &d.samples, &d.unit, &wls); err != nil {
		return nil, fmt.Errorf("profile totals: %w", err)
	}
	for _, x := range wls {
		if len(x) == 3 {
			d.workload = append(d.workload, wl{x[0], x[1], x[2]})
		}
	}
	if d.samples == 0 {
		return d, nil
	}
	rows, err := s.CH.QueryContext(qctx, `
		SELECT stack_hash, sum(value) AS v FROM profile_samples WHERE `+w.SQL()+`
		GROUP BY stack_hash ORDER BY v DESC LIMIT ?`, append(w.Args(), s.maxStacks())...)
	if err != nil {
		return nil, fmt.Errorf("profile stacks: %w", err)
	}
	type hv struct {
		hash uint64
		v    int64
	}
	var top []hv
	for rows.Next() {
		var x hv
		if err := rows.Scan(&x.hash, &x.v); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("profile stacks scan: %w", err)
		}
		top = append(top, x)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	frames := make(map[uint64][]string, len(top))
	for i := 0; i < len(top); i += frameChunk {
		j := min(i+frameChunk, len(top))
		hashes := make([]uint64, 0, j-i)
		for _, x := range top[i:j] {
			hashes = append(hashes, x.hash)
		}
		fr, err := s.CH.QueryContext(qctx,
			`SELECT stack_hash, any(frames) FROM profile_stacks WHERE stack_hash IN (?) GROUP BY stack_hash`, hashes)
		if err != nil {
			return nil, fmt.Errorf("profile frames: %w", err)
		}
		for fr.Next() {
			var (
				h uint64
				f []string
			)
			if err := fr.Scan(&h, &f); err != nil {
				_ = fr.Close()
				return nil, fmt.Errorf("profile frames scan: %w", err)
			}
			frames[h] = f
		}
		if err := fr.Close(); err != nil {
			return nil, err
		}
	}
	var covered int64
	for _, x := range top {
		f := frames[x.hash]
		if len(f) == 0 {
			// Stack evicted or not yet written: keep the time visible.
			f = []string{unknownStack}
		}
		d.stacks = append(d.stacks, Stack{Frames: f, Value: x.v})
		covered += x.v
	}
	if rest := d.total - covered; rest > 0 {
		d.stacks = append(d.stacks, Stack{Frames: []string{otherStacks}, Value: rest})
	}
	return d, nil
}

// cpuCostMonth prices the profiled CPU: the profiled workloads' CPU $
// over the window, scaled to a 30-day month. A pod selector prices
// just that pod from pod_cost_1s.
func (s *Service) cpuCostMonth(ctx context.Context, sel selector, d *profileData, start, end time.Time) (float64, error) {
	if sel.typ != "cpu" || len(d.workload) == 0 {
		return 0, nil
	}
	covered := end.Sub(start)
	qctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	if sel.pod != "" {
		var w chsql.Where
		w.Add("ts >= ? AND ts < ?", start.UnixMilli(), end.UnixMilli())
		w.Add("pod = ?", sel.pod)
		if sel.namespace != "" {
			w.Add("namespace = ?", sel.namespace)
		}
		if sel.clusterAliases != nil {
			w.In("cluster_id", sel.clusterAliases)
		}
		var usd float64
		if err := s.CH.QueryRowContext(qctx, `SELECT sum(cpu_cost_usd_sec * interval_sec) FROM pod_cost_1s WHERE `+w.SQL(),
			w.Args()...).Scan(&usd); err != nil {
			return 0, err
		}
		return timewin.PerMonth(usd, covered), nil
	}
	want := map[wl]bool{}
	var cls, nss, wls []string
	for _, x := range d.workload {
		want[x] = true
		cls, nss, wls = appendU(cls, x.cluster), appendU(nss, x.namespace), appendU(wls, x.workload)
	}
	hs, he := timewin.FloorHour(start), timewin.CeilHour(end)
	var w chsql.Where
	w.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", hs.Unix(), he.Unix())
	w.In("cluster_id", cls)
	w.In("namespace", nss)
	w.In("workload", wls)
	rows, err := s.CH.QueryContext(qctx, `SELECT cluster_id, namespace, workload, sum(cpu_cost_usd) FROM workload_cost_1h
		WHERE `+w.SQL()+` GROUP BY cluster_id, namespace, workload`, w.Args()...)
	if err != nil {
		return 0, err
	}
	defer rows.Close() //nolint:errcheck
	var usd float64
	for rows.Next() {
		var (
			x wl
			v float64
		)
		if err := rows.Scan(&x.cluster, &x.namespace, &x.workload, &v); err != nil {
			return 0, err
		}
		if want[x] {
			usd += v
		}
	}
	span := he.Sub(hs)
	if now := s.now(); he.After(now) {
		span = now.Sub(hs)
	}
	return timewin.PerMonth(usd, span), rows.Err()
}

func appendU(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// ─── RPCs ────────────────────────────────────────────────────────────

// ListProfileTargets lists services with profiles in the window.
func (s *Service) ListProfileTargets(ctx context.Context, req *connect.Request[kuberov1.ListProfileTargetsRequest]) (*connect.Response[kuberov1.ListProfileTargetsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	w, err := timewin.FromUnixMS(m.GetStartUnixMs(), m.GetEndUnixMs(), defaultSpan, maxSpan, s.now())
	if err != nil {
		return nil, invalid(err)
	}
	snap := s.Clusters.Snapshot(ctx)
	cluster, err := clusters.Scope(ctx, snap, strings.TrimSpace(m.GetClusterId()))
	if err != nil {
		return nil, err
	}
	ns := strings.TrimSpace(m.GetNamespace())
	if len(ns) > 63 {
		return nil, invalid(errors.New("namespace too long"))
	}
	if !s.live() {
		if err := s.demoGate("ListProfileTargets"); err != nil {
			return nil, err
		}
		return connect.NewResponse(&kuberov1.ListProfileTargetsResponse{Targets: demoTargets(ns, w.End)}), nil
	}
	var cond chsql.Where
	cond.Add("ts >= toDateTime(?) AND ts < toDateTime(?)", w.Start.Unix(), w.QueryEnd().Unix())
	if cl := snap.Aliases(cluster); cl != nil {
		cond.In("cluster_id", cl)
	}
	if ns != "" {
		cond.Add("namespace = ?", ns)
	}
	qctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	rows, err := s.CH.QueryContext(qctx, `
		SELECT cluster_id, service, namespace, workload, groupUniqArray(type), argMax(origin, ts),
		       toInt64(toUnixTimestamp(max(ts))), sumIf(value, type = 'cpu' AND unit = 'nanoseconds')
		FROM profile_samples WHERE `+cond.SQL()+`
		GROUP BY cluster_id, service, namespace, workload
		LIMIT 5000`, cond.Args()...)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("profile targets: %w", err))
	}
	type key struct{ service, namespace, workload string }
	targets := map[key]*kuberov1.ProfileTarget{}
	var wlCost []wl
	covered := w.Covered()
	for rows.Next() {
		var (
			cl, svc, n, wkl, origin string
			types                   []string
			last                    int64
			cpuNS                   int64
		)
		if err := rows.Scan(&cl, &svc, &n, &wkl, &types, &origin, &last, &cpuNS); err != nil {
			_ = rows.Close()
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		k := key{svc, n, wkl}
		t := targets[k]
		if t == nil {
			t = &kuberov1.ProfileTarget{Service: svc, Namespace: n, Workload: wkl, Origin: origin}
			targets[k] = t
		}
		for _, ty := range types {
			if !contains(t.Types, ty) {
				t.Types = append(t.Types, ty)
			}
		}
		if last*1000 > t.LastSeenUnixMs {
			t.LastSeenUnixMs, t.Origin = last*1000, origin
		}
		if covered > 0 {
			t.CpuCoresAvg += float64(cpuNS) / float64(covered.Nanoseconds())
		}
		wlCost = append(wlCost, wl{cl, n, wkl})
	}
	if err := rows.Close(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	costs, err := s.workloadCostMonth(ctx, wlCost, w)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*kuberov1.ProfileTarget, 0, len(targets))
	for k, t := range targets {
		for x, v := range costs {
			if x.namespace == k.namespace && x.workload == k.workload {
				t.CostUsdMonth += v
			}
		}
		t.CostUsdMonth = math.Round(t.CostUsdMonth*100) / 100
		sort.Strings(t.Types)
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostUsdMonth != out[j].CostUsdMonth {
			return out[i].CostUsdMonth > out[j].CostUsdMonth
		}
		return out[i].Service < out[j].Service
	})
	return connect.NewResponse(&kuberov1.ListProfileTargetsResponse{Targets: out}), nil
}

// workloadCostMonth returns total $/mo per workload over the window.
func (s *Service) workloadCostMonth(ctx context.Context, wls []wl, w timewin.Window) (map[wl]float64, error) {
	out := map[wl]float64{}
	if len(wls) == 0 {
		return out, nil
	}
	want := map[wl]bool{}
	var cls, nss, names []string
	for _, x := range wls {
		if x.workload == "" {
			continue
		}
		want[x] = true
		cls, nss, names = appendU(cls, x.cluster), appendU(nss, x.namespace), appendU(names, x.workload)
	}
	if len(want) == 0 {
		return out, nil
	}
	hs, he := timewin.FloorHour(w.Start), timewin.CeilHour(w.QueryEnd())
	var cond chsql.Where
	cond.Add("ts_hour >= toDateTime(?) AND ts_hour < toDateTime(?)", hs.Unix(), he.Unix())
	cond.In("cluster_id", cls)
	cond.In("namespace", nss)
	cond.In("workload", names)
	qctx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()
	rows, err := s.CH.QueryContext(qctx, `SELECT cluster_id, namespace, workload, sum(cost_usd) FROM workload_cost_1h
		WHERE `+cond.SQL()+` GROUP BY cluster_id, namespace, workload`, cond.Args()...)
	if err != nil {
		return nil, fmt.Errorf("profile target cost: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	span := he.Sub(hs)
	if now := s.now(); he.After(now) {
		span = now.Sub(hs)
	}
	for rows.Next() {
		var (
			x wl
			v float64
		)
		if err := rows.Scan(&x.cluster, &x.namespace, &x.workload, &v); err != nil {
			return nil, err
		}
		if want[x] {
			out[x] += timewin.PerMonth(v, span)
		}
	}
	return out, rows.Err()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// GetFlamegraph returns a merged (or diff) flamegraph.
func (s *Service) GetFlamegraph(ctx context.Context, req *connect.Request[kuberov1.GetFlamegraphRequest]) (*connect.Response[kuberov1.GetFlamegraphResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	sel, err := s.parseSelector(ctx, m.GetSelector())
	if err != nil {
		return nil, err
	}
	w, err := timewin.FromUnixMS(m.GetStartUnixMs(), m.GetEndUnixMs(), defaultSpan, maxSpan, s.now())
	if err != nil {
		return nil, invalid(err)
	}
	diff := m.GetBaselineStartUnixMs() > 0 && m.GetBaselineEndUnixMs() > 0
	var bw timewin.Window
	if diff {
		if bw, err = timewin.FromUnixMS(m.GetBaselineStartUnixMs(), m.GetBaselineEndUnixMs(), defaultSpan, maxSpan, s.now()); err != nil {
			return nil, invalid(fmt.Errorf("baseline: %w", err))
		}
	}
	maxNodes := int(m.GetMaxNodes())
	if maxNodes <= 0 {
		maxNodes = defaultMaxNodes
	}
	maxNodes = max(16, min(maxNodes, maxMaxNodes))

	if !s.live() {
		if err := s.demoGate("GetFlamegraph"); err != nil {
			return nil, err
		}
		return connect.NewResponse(demoFlamegraph(sel.typ, diff, maxNodes)), nil
	}
	cur, err := s.load(ctx, sel, w.Start, w.QueryEnd())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	tree := NewTree()
	for _, st := range cur.stacks {
		tree.Add(st.Frames, st.Value)
	}
	if diff {
		base, err := s.load(ctx, sel, bw.Start, bw.QueryEnd())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		for _, st := range base.stacks {
			tree.AddBaseline(st.Frames, st.Value)
		}
	}
	cost, err := s.cpuCostMonth(ctx, sel, cur, w.Start, w.QueryEnd())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("profile pricing: %w", err))
	}
	return connect.NewResponse(flameResponse(tree, maxNodes, cur.unit, sel.typ, cur.samples, cost)), nil
}

func flameResponse(tree *Tree, maxNodes int, unit, typ string, samples int64, cost float64) *kuberov1.GetFlamegraphResponse {
	flat := tree.Flatten(maxNodes)
	resp := &kuberov1.GetFlamegraphResponse{
		Total: tree.Total, Unit: unit, Type: typ, BaselineTotal: tree.BaselineTotal, Samples: samples,
		CostUsdMonth: math.Round(cost*100) / 100,
		Nodes:        make([]*kuberov1.FlameNode, 0, len(flat)),
	}
	for _, n := range flat {
		resp.Nodes = append(resp.Nodes, &kuberov1.FlameNode{Name: n.Name, Parent: n.Parent, Depth: n.Depth,
			Self: n.Self, Total: n.Total, BaselineSelf: n.BaselineSelf, BaselineTotal: n.BaselineTotal})
	}
	return resp
}

// GetTopFunctions ranks functions by self or total time with $/mo.
func (s *Service) GetTopFunctions(ctx context.Context, req *connect.Request[kuberov1.GetTopFunctionsRequest]) (*connect.Response[kuberov1.GetTopFunctionsResponse], error) {
	if err := auth.Require(ctx, auth.RoleViewer); err != nil {
		return nil, err
	}
	m := req.Msg
	sel, err := s.parseSelector(ctx, m.GetSelector())
	if err != nil {
		return nil, err
	}
	w, err := timewin.FromUnixMS(m.GetStartUnixMs(), m.GetEndUnixMs(), defaultSpan, maxSpan, s.now())
	if err != nil {
		return nil, invalid(err)
	}
	limit := int(m.GetLimit())
	if limit <= 0 {
		limit = 25
	}
	limit = min(limit, 500)
	by := strings.ToLower(strings.TrimSpace(m.GetOrderBy()))
	switch by {
	case "":
		by = "self"
	case "self", "total":
	default:
		return nil, invalid(errors.New("order_by must be self or total"))
	}
	var (
		d    *profileData
		cost float64
	)
	if !s.live() {
		if err := s.demoGate("GetTopFunctions"); err != nil {
			return nil, err
		}
		d, cost = demoProfile(sel.typ, true)
	} else {
		if d, err = s.load(ctx, sel, w.Start, w.QueryEnd()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if cost, err = s.cpuCostMonth(ctx, sel, d, w.Start, w.QueryEnd()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("profile pricing: %w", err))
		}
	}
	return connect.NewResponse(topResponse(d, by, limit, cost)), nil
}

func topResponse(d *profileData, by string, limit int, cost float64) *kuberov1.GetTopFunctionsResponse {
	resp := &kuberov1.GetTopFunctionsResponse{Total: d.total, Unit: d.unit}
	for _, f := range TopFunctions(d.stacks, by, limit) {
		tf := &kuberov1.TopFunction{Name: f.Name, Self: f.Self, Total: f.Total}
		if d.total > 0 {
			tf.SelfPct = 100 * float64(f.Self) / float64(d.total)
			tf.TotalPct = 100 * float64(f.Total) / float64(d.total)
			tf.SelfCostUsdMonth = math.Round(cost*float64(f.Self)/float64(d.total)*100) / 100
		}
		resp.Functions = append(resp.Functions, tf)
	}
	return resp
}
