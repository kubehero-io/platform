// SPDX-License-Identifier: Apache-2.0
// Copyright (c) KubeHero contributors

// Package mcpserver exposes KubeHero to MCP clients (Claude Desktop,
// Claude Code, any MCP host) as READ-ONLY tools over the same Connect
// RPCs the CLI uses, authenticated with the CLI's configured token.
//
// There are deliberately no mutating tools: an assistant can look at
// cost, logs, profiles, the network map, alerts and rightsizing, and it
// can ask the advisor to investigate — but changing anything stays a
// human act through KubeHero's arming flow.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/kubehero-io/platform/cli/kubehero/internal/rpc"
	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

const (
	// maxResultBytes caps one tool result: MCP hosts put results in the
	// model's context, which should stay small.
	maxResultBytes = 16000
	maxLogLines    = 100
	maxLineRunes   = 500
	maxQueryLen    = 1000
	callTimeout    = 30 * time.Second
	// investigateTimeout covers the advisor's LLM loop (120s) + fallback.
	investigateTimeout = 4 * time.Minute
)

// New builds the MCP server over the given clients.
func New(cl *rpc.Clients, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "kubehero", Title: "KubeHero", Version: version}, &mcp.ServerOptions{
		Instructions: "KubeHero is a Kubernetes cost, observability and optimization platform. These tools are " +
			"read-only views over the user's fleet: cost allocation and trends, rightsizing, logs (LogQL), " +
			"continuous profiles, the eBPF service map and network costs, alerts and anomalies. For open " +
			"questions, `investigate` runs KubeHero's own investigation agent. Nothing here changes the cluster; " +
			"proposals come back as policy manifests a human applies through KubeHero's arming flow.",
	})
	h := &handlers{cl: cl, now: time.Now}
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	add := func(name, title, desc string) *mcp.Tool {
		return &mcp.Tool{Name: name, Title: title, Description: desc, Annotations: readOnly}
	}

	mcp.AddTool(s, add("list_clusters", "List clusters",
		"Clusters registered with KubeHero: id, name, cloud, region, node count."), h.listClusters)
	mcp.AddTool(s, add("get_cost_allocation", "Cost allocation",
		"Cost allocation (OpenCost-compatible) over a window, grouped by up to 3 dimensions, with CPU/RAM/GPU/network/idle "+
			"cost, usage efficiency and recoverable cost per row."), h.costAllocation)
	mcp.AddTool(s, add("get_cost_timeseries", "Spend over time",
		"Spend per step (1h or 1d), optionally grouped, plus a month-end forecast. Use it to see what changed and when."), h.costTimeseries)
	mcp.AddTool(s, add("list_rightsizing", "Rightsizing recommendations",
		"Per-container rightsizing from measured usage: current vs recommended CPU/memory requests, $/mo savings (negative = "+
			"under-provisioned), confidence and OOM kills."), h.rightsizing)
	mcp.AddTool(s, add("query_logs", "Query logs (LogQL)",
		`Run a LogQL query. Log queries return lines (newest first, at most 100, bodies truncated); metric queries return series. `+
			`Examples: {namespace="payments", level="error"} |= "timeout" · sum by (namespace) (count_over_time({level="error"}[5m])).`), h.queryLogs)
	mcp.AddTool(s, add("get_log_patterns", "Log patterns",
		"Cluster matching log lines into templates (<_> marks the variable part) with counts, share and a sample line."), h.logPatterns)
	mcp.AddTool(s, add("get_top_functions", "Top functions (profiling)",
		"Continuous-profiling top functions for a service ranked by self or total time, with the $/mo each function's CPU costs."), h.topFunctions)
	mcp.AddTool(s, add("get_service_map", "Service map",
		"eBPF service map edges (who talks to whom) ranked by $/mo, with bytes, cross-zone / egress flags and TCP retransmits."), h.serviceMap)
	mcp.AddTool(s, add("list_network_costs", "Network costs",
		"Per-workload network spend: internet egress and cross-zone transfer in $/mo and GB, with each workload's top destination."), h.networkCosts)
	mcp.AddTool(s, add("list_alerts", "Alerts",
		"Alerts from KubeHero's alerting engine (logs, cost, budget, anomaly, network, event rules) with state, severity and labels."), h.alerts)
	mcp.AddTool(s, add("list_anomalies", "Anomalies",
		"Statistically anomalous spend / capacity / posture signals ranked by $/mo impact."), h.anomalies)
	mcp.AddTool(s, add("get_briefing", "Advisor briefing",
		"The advisor's current briefing: headline, markdown report, spoken script and proposed guarded actions (each with a "+
			"policy CRD manifest a human applies through the arming flow)."), h.briefing)
	mcp.AddTool(s, add("investigate", "Investigate a question",
		"Ask KubeHero's investigation agent a free-form question (e.g. \"why did checkout's spend jump last night?\"). It runs "+
			"its own read-only tools over every signal and returns an answer with cited evidence, the steps it took, and guarded "+
			"proposals. Can take up to a couple of minutes."), h.investigate)
	return s
}

func ptr[T any](v T) *T { return &v }

type handlers struct {
	cl  *rpc.Clients
	now func() time.Time
}

// ─── inputs ──────────────────────────────────────────────────────────────

type noInput struct{}

type allocationInput struct {
	Window      string            `json:"window,omitempty" jsonschema:"window: 24h, 7d (default), 30d, today, yesterday, week, month or lastmonth"`
	Aggregate   []string          `json:"aggregate,omitempty" jsonschema:"group by up to 3 of: cluster, namespace (default), workload, controller, node, nodepool, team, cost_center, zone"`
	Filters     map[string]string `json:"filters,omitempty" jsonschema:"exact-match filters on the same dimensions, e.g. {\"namespace\": \"payments\"}"`
	IncludeIdle bool              `json:"include_idle,omitempty" jsonschema:"add an __idle__ row per cluster"`
	Limit       int               `json:"limit,omitempty" jsonschema:"max rows, 1-50 (default 20)"`
}

type timeseriesInput struct {
	Window  string            `json:"window,omitempty" jsonschema:"window: 24h, 7d or 30d (default)"`
	Step    string            `json:"step,omitempty" jsonschema:"resolution: 1h or 1d (default auto)"`
	GroupBy string            `json:"group_by,omitempty" jsonschema:"namespace, team, cluster, nodepool or workload; empty = total"`
	Filters map[string]string `json:"filters,omitempty" jsonschema:"exact-match filters, e.g. {\"namespace\": \"payments\"}"`
	Top     int               `json:"top,omitempty" jsonschema:"keep the N largest series, 1-10 (default 5)"`
}

type rightsizingInput struct {
	Namespace  string  `json:"namespace,omitempty" jsonschema:"limit to one namespace"`
	Window     string  `json:"window,omitempty" jsonschema:"observation window: 7d (default), 14d or 30d"`
	MinSavings float64 `json:"min_savings_usd_month,omitempty" jsonschema:"only rows saving at least this many dollars per month"`
	Limit      int     `json:"limit,omitempty" jsonschema:"max rows, 1-50 (default 20)"`
}

type logsInput struct {
	Query string `json:"query" jsonschema:"LogQL query (max 1000 chars)"`
	Since string `json:"since,omitempty" jsonschema:"lookback: 5m, 15m, 1h (default), 6h, 24h or 7d"`
	Limit int    `json:"limit,omitempty" jsonschema:"max lines or patterns, 1-100 (default 50)"`
}

type topFunctionsInput struct {
	Service   string `json:"service" jsonschema:"service / workload name"`
	Namespace string `json:"namespace,omitempty" jsonschema:"namespace, when the name is ambiguous"`
	Type      string `json:"type,omitempty" jsonschema:"profile type: cpu (default), alloc_space, alloc_objects, inuse_space, inuse_objects, goroutine, mutex, block"`
	Since     string `json:"since,omitempty" jsonschema:"lookback: 5m, 15m, 1h (default), 6h, 24h or 7d"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max functions, 1-50 (default 20)"`
}

type serviceMapInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"focus namespace"`
	Since     string `json:"since,omitempty" jsonschema:"lookback: 5m, 15m, 1h (default), 6h, 24h or 7d"`
	MaxEdges  int    `json:"max_edges,omitempty" jsonschema:"max edges, 1-50 (default 20)"`
}

type networkCostsInput struct {
	Since string `json:"since,omitempty" jsonschema:"lookback: 1h, 6h, 24h (default) or 7d"`
	Limit int    `json:"limit,omitempty" jsonschema:"max rows, 1-50 (default 20)"`
}

type alertsInput struct {
	State string `json:"state,omitempty" jsonschema:"pending, firing or resolved; empty = all"`
	Limit int    `json:"limit,omitempty" jsonschema:"max alerts, 1-100 (default 50)"`
}

type anomaliesInput struct {
	Window string `json:"window,omitempty" jsonschema:"24h, 7d (default) or 30d"`
	Limit  int    `json:"limit,omitempty" jsonschema:"max anomalies, 1-50 (default 20)"`
}

type briefingInput struct {
	ClusterID string `json:"cluster_id,omitempty" jsonschema:"cluster id; empty = fleet-wide"`
	Window    string `json:"window,omitempty" jsonschema:"24h (default) or 7d"`
}

type investigateInput struct {
	Question  string `json:"question" jsonschema:"the question to investigate"`
	ClusterID string `json:"cluster_id,omitempty" jsonschema:"cluster id; empty = fleet-wide"`
	Window    string `json:"window,omitempty" jsonschema:"1h, 24h (default) or 7d"`
	Context   string `json:"context,omitempty" jsonschema:"dashboard path the question is about, e.g. /workloads/<cluster>/<namespace>/<name>"`
}

// ─── validation ──────────────────────────────────────────────────────────

var (
	sinceDur = map[string]time.Duration{
		"5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour,
		"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
	}
	allocWindows = set("24h", "7d", "30d", "today", "yesterday", "week", "month", "lastmonth")
	allocDims    = set("cluster", "namespace", "workload", "controller", "node", "nodepool", "team", "cost_center", "zone")
	dnsLabelRE   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	serviceRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
)

func set(vals ...string) map[string]bool {
	m := map[string]bool{}
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func oneOf(field, v string, allowed ...string) error {
	for _, a := range allowed {
		if v == a {
			return nil
		}
	}
	return fmt.Errorf("%s %q must be one of: %s", field, v, strings.Join(allowed, ", "))
}

func clamp(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(v, hi))
}

func (h *handlers) span(since, def string) (int64, int64, error) {
	if since == "" {
		since = def
	}
	d, ok := sinceDur[since]
	if !ok {
		return 0, 0, fmt.Errorf("since %q must be one of: 5m, 15m, 1h, 6h, 24h, 7d", since)
	}
	now := h.now()
	return now.Add(-d).UnixMilli(), now.UnixMilli(), nil
}

func validText(field, v string, maxLen int) error {
	if len(v) > maxLen {
		return fmt.Errorf("%s exceeds %d characters", field, maxLen)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\t' && r != '\n' }) {
		return fmt.Errorf("%s contains control characters", field)
	}
	return nil
}

func validFilters(f map[string]string) error {
	if len(f) > 4 {
		return errors.New("at most 4 filters")
	}
	for k, v := range f {
		if !allocDims[k] {
			return fmt.Errorf("filter key %q is not a cost dimension", k)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("filter %s is empty", k)
		}
		if err := validText("filter "+k, v, 128); err != nil {
			return err
		}
	}
	return nil
}

func validNamespace(ns string) error {
	if ns != "" && !dnsLabelRE.MatchString(ns) {
		return fmt.Errorf("namespace %q is not a valid namespace name", ns)
	}
	return nil
}

func validLogQL(q string) error {
	if strings.TrimSpace(q) == "" {
		return errors.New("query is required")
	}
	if err := validText("query", q, maxQueryLen); err != nil {
		return err
	}
	if !strings.Contains(q, "{") || !strings.Contains(q, "}") {
		return errors.New(`query must contain a LogQL stream selector, e.g. {namespace="payments"}`)
	}
	return nil
}

// ─── results ─────────────────────────────────────────────────────────────

// result renders a proto response as compact JSON text content, marked
// partial when it exceeds the size budget.
func result(m proto.Message) (*mcp.CallToolResult, any, error) {
	b, err := protojson.MarshalOptions{EmitUnpopulated: false}.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	text := string(b)
	if len(b) > maxResultBytes {
		cut := maxResultBytes - 200
		for cut > 0 && !utf8.RuneStart(b[cut]) {
			cut--
		}
		text = string(b[:cut]) + "\n…[truncated: result exceeded the size budget; narrow the query or lower the limit]"
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, callTimeout)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ─── handlers ────────────────────────────────────────────────────────────

func (h *handlers) listClusters(ctx context.Context, _ *mcp.CallToolRequest, _ noInput) (*mcp.CallToolResult, any, error) {
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Control.ListClusters(ctx, connect.NewRequest(&kuberov1.ListClustersRequest{PageSize: 200}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) costAllocation(ctx context.Context, _ *mcp.CallToolRequest, in allocationInput) (*mcp.CallToolResult, any, error) {
	if in.Window == "" {
		in.Window = "7d"
	}
	if !allocWindows[in.Window] {
		return nil, nil, fmt.Errorf("window %q is not supported", in.Window)
	}
	if len(in.Aggregate) == 0 {
		in.Aggregate = []string{"namespace"}
	}
	if len(in.Aggregate) > 3 {
		return nil, nil, errors.New("aggregate takes at most 3 dimensions")
	}
	for _, d := range in.Aggregate {
		if !allocDims[d] {
			return nil, nil, fmt.Errorf("aggregate %q is not a cost dimension", d)
		}
	}
	if err := validFilters(in.Filters); err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Cost.GetAllocation(ctx, connect.NewRequest(&kuberov1.GetAllocationRequest{
		Window: in.Window, Aggregate: in.Aggregate, Filters: in.Filters, IncludeIdle: in.IncludeIdle,
	}))
	if err != nil {
		return nil, nil, err
	}
	if n := clamp(in.Limit, 20, 1, 50); len(res.Msg.Allocations) > n {
		res.Msg.Allocations = res.Msg.Allocations[:n]
	}
	return result(res.Msg)
}

func (h *handlers) costTimeseries(ctx context.Context, _ *mcp.CallToolRequest, in timeseriesInput) (*mcp.CallToolResult, any, error) {
	if in.Window == "" {
		in.Window = "30d"
	}
	if err := oneOf("window", in.Window, "24h", "7d", "30d"); err != nil {
		return nil, nil, err
	}
	if err := oneOf("step", in.Step, "", "1h", "1d"); err != nil {
		return nil, nil, err
	}
	if err := oneOf("group_by", in.GroupBy, "", "namespace", "team", "cluster", "nodepool", "workload"); err != nil {
		return nil, nil, err
	}
	if err := validFilters(in.Filters); err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Cost.GetCostTimeseries(ctx, connect.NewRequest(&kuberov1.GetCostTimeseriesRequest{
		Window: in.Window, Step: in.Step, GroupBy: in.GroupBy, Filters: in.Filters, Top: int32(clamp(in.Top, 5, 1, 10)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) rightsizing(ctx context.Context, _ *mcp.CallToolRequest, in rightsizingInput) (*mcp.CallToolResult, any, error) {
	if in.Window == "" {
		in.Window = "7d"
	}
	if err := oneOf("window", in.Window, "7d", "14d", "30d"); err != nil {
		return nil, nil, err
	}
	if err := validNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}
	if in.MinSavings < 0 {
		return nil, nil, errors.New("min_savings_usd_month must be >= 0")
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Cost.ListRightsizing(ctx, connect.NewRequest(&kuberov1.ListRightsizingRequest{
		Namespace: in.Namespace, Window: in.Window, MinSavingsUsdMonth: in.MinSavings, Limit: int32(clamp(in.Limit, 20, 1, 50)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) queryLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, any, error) {
	if err := validLogQL(in.Query); err != nil {
		return nil, nil, err
	}
	start, end, err := h.span(in.Since, "1h")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	limit := clamp(in.Limit, 50, 1, maxLogLines)
	res, err := h.cl.Logs.QueryLogs(ctx, connect.NewRequest(&kuberov1.QueryLogsRequest{
		Query: in.Query, StartUnixMs: start, EndUnixMs: end, Limit: int32(limit), Direction: "backward",
	}))
	if err != nil {
		return nil, nil, err
	}
	if len(res.Msg.Lines) > limit {
		res.Msg.Lines = res.Msg.Lines[:limit]
	}
	for _, l := range res.Msg.Lines {
		l.Body = truncateRunes(l.Body, maxLineRunes)
	}
	return result(res.Msg)
}

func (h *handlers) logPatterns(ctx context.Context, _ *mcp.CallToolRequest, in logsInput) (*mcp.CallToolResult, any, error) {
	if err := validLogQL(in.Query); err != nil {
		return nil, nil, err
	}
	start, end, err := h.span(in.Since, "1h")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Logs.GetLogPatterns(ctx, connect.NewRequest(&kuberov1.GetLogPatternsRequest{
		Query: in.Query, StartUnixMs: start, EndUnixMs: end, Limit: int32(clamp(in.Limit, 20, 1, 50)),
	}))
	if err != nil {
		return nil, nil, err
	}
	for _, p := range res.Msg.Patterns {
		p.Sample = truncateRunes(p.Sample, maxLineRunes)
		p.Trend = nil // sparkline data; noise for a model
	}
	return result(res.Msg)
}

func (h *handlers) topFunctions(ctx context.Context, _ *mcp.CallToolRequest, in topFunctionsInput) (*mcp.CallToolResult, any, error) {
	if !serviceRE.MatchString(in.Service) {
		return nil, nil, errors.New("service is required (a workload / service name)")
	}
	if err := validNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}
	if in.Type == "" {
		in.Type = "cpu"
	}
	if err := oneOf("type", in.Type, "cpu", "alloc_space", "alloc_objects", "inuse_space", "inuse_objects", "goroutine", "mutex", "block"); err != nil {
		return nil, nil, err
	}
	start, end, err := h.span(in.Since, "1h")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Profiles.GetTopFunctions(ctx, connect.NewRequest(&kuberov1.GetTopFunctionsRequest{
		Selector:    &kuberov1.ProfileSelector{Service: in.Service, Namespace: in.Namespace, Type: in.Type},
		StartUnixMs: start, EndUnixMs: end, Limit: int32(clamp(in.Limit, 20, 1, 50)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) serviceMap(ctx context.Context, _ *mcp.CallToolRequest, in serviceMapInput) (*mcp.CallToolResult, any, error) {
	if err := validNamespace(in.Namespace); err != nil {
		return nil, nil, err
	}
	start, end, err := h.span(in.Since, "1h")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Network.GetServiceMap(ctx, connect.NewRequest(&kuberov1.GetServiceMapRequest{
		Namespace: in.Namespace, StartUnixMs: start, EndUnixMs: end,
	}))
	if err != nil {
		return nil, nil, err
	}
	edges := res.Msg.Edges
	sortEdges(edges)
	if n := clamp(in.MaxEdges, 20, 1, 50); len(edges) > n {
		res.Msg.Edges = edges[:n]
	}
	return result(res.Msg)
}

func sortEdges(es []*kuberov1.ServiceMapEdge) {
	sort.SliceStable(es, func(i, j int) bool { return es[i].GetCostUsdMonth() > es[j].GetCostUsdMonth() })
}

func (h *handlers) networkCosts(ctx context.Context, _ *mcp.CallToolRequest, in networkCostsInput) (*mcp.CallToolResult, any, error) {
	start, end, err := h.span(in.Since, "24h")
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Network.ListNetworkCosts(ctx, connect.NewRequest(&kuberov1.ListNetworkCostsRequest{
		StartUnixMs: start, EndUnixMs: end, Limit: int32(clamp(in.Limit, 20, 1, 50)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) alerts(ctx context.Context, _ *mcp.CallToolRequest, in alertsInput) (*mcp.CallToolResult, any, error) {
	if err := oneOf("state", in.State, "", "pending", "firing", "resolved"); err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Alerts.ListAlerts(ctx, connect.NewRequest(&kuberov1.ListAlertsRequest{
		State: in.State, Limit: int32(clamp(in.Limit, 50, 1, 100)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) anomalies(ctx context.Context, _ *mcp.CallToolRequest, in anomaliesInput) (*mcp.CallToolResult, any, error) {
	if in.Window == "" {
		in.Window = "7d"
	}
	if err := oneOf("window", in.Window, "24h", "7d", "30d"); err != nil {
		return nil, nil, err
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	res, err := h.cl.Control.ListAnomalies(ctx, connect.NewRequest(&kuberov1.ListAnomaliesRequest{
		Window: in.Window, Limit: int32(clamp(in.Limit, 20, 1, 50)),
	}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) briefing(ctx context.Context, _ *mcp.CallToolRequest, in briefingInput) (*mcp.CallToolResult, any, error) {
	if in.Window == "" {
		in.Window = "24h"
	}
	if err := oneOf("window", in.Window, "24h", "7d"); err != nil {
		return nil, nil, err
	}
	if err := validText("cluster_id", in.ClusterID, 128); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, investigateTimeout)
	defer cancel()
	res, err := h.cl.Advisor.GetBriefing(ctx, connect.NewRequest(&kuberov1.GetBriefingRequest{ClusterId: in.ClusterID, Window: in.Window}))
	if err != nil {
		return nil, nil, err
	}
	return result(res.Msg)
}

func (h *handlers) investigate(ctx context.Context, _ *mcp.CallToolRequest, in investigateInput) (*mcp.CallToolResult, any, error) {
	q := strings.TrimSpace(in.Question)
	if q == "" {
		return nil, nil, errors.New("question is required")
	}
	if err := validText("question", q, 2000); err != nil {
		return nil, nil, err
	}
	if err := validText("context", in.Context, 512); err != nil {
		return nil, nil, err
	}
	if in.Window == "" {
		in.Window = "24h"
	}
	if err := oneOf("window", in.Window, "1h", "24h", "7d"); err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, investigateTimeout)
	defer cancel()
	res, err := h.cl.Advisor.Investigate(ctx, connect.NewRequest(&kuberov1.InvestigateRequest{
		Question: q, ClusterId: in.ClusterID, Window: in.Window, Context: in.Context,
	}))
	if err != nil {
		return nil, nil, err
	}
	// Steps carry full tool inputs; keep only what a reader needs.
	for _, s := range res.Msg.Steps {
		s.InputJson = truncateRunes(s.InputJson, 300)
	}
	return result(res.Msg)
}

// ToolNames lists the registered tools (docs + tests).
var ToolNames = []string{
	"list_clusters", "get_cost_allocation", "get_cost_timeseries", "list_rightsizing", "query_logs",
	"get_log_patterns", "get_top_functions", "get_service_map", "list_network_costs", "list_alerts",
	"list_anomalies", "get_briefing", "investigate",
}
