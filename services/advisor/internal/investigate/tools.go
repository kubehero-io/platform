// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package investigate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

const (
	// DefaultMaxCalls is the per-investigation tool-call budget.
	DefaultMaxCalls = 12
	// maxResultBytes caps one tool result handed to the model: the
	// context stays small and one noisy query can't crowd out the rest.
	maxResultBytes = 8000
	maxLogLines    = 50
	maxLineRunes   = 300
	maxQueryLen    = 1000
)

// ErrBudget is returned once an investigation has spent its tool calls.
var ErrBudget = errors.New("tool-call budget exhausted: answer with the evidence gathered so far")

// Spec describes one tool to the model.
type Spec struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema of the input object
}

// Toolbox runs read-only tools for one investigation: it validates
// inputs, enforces the call budget, records every call as an
// InvestigateStep (streamed live through the Emitter) and trims results.
// Safe for concurrent use.
type Toolbox struct {
	backend  backend.Backend
	req      Request
	now      time.Time
	emit     Emitter
	maxCalls int

	mu    sync.Mutex
	calls int
	steps []*kuberov1.InvestigateStep
}

// NewToolbox binds tools to a backend and a request.
func NewToolbox(b backend.Backend, req Request, now time.Time, maxCalls int, emit Emitter) *Toolbox {
	if maxCalls <= 0 {
		maxCalls = DefaultMaxCalls
	}
	return &Toolbox{backend: b, req: req, now: now, emit: emit, maxCalls: maxCalls}
}

// Steps returns the recorded steps in call order.
func (t *Toolbox) Steps() []*kuberov1.InvestigateStep {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]*kuberov1.InvestigateStep, len(t.steps))
	copy(out, t.steps)
	return out
}

// Specs lists every tool, in a stable order.
func Specs() []Spec {
	out := make([]Spec, len(toolDefs))
	for i, d := range toolDefs {
		out[i] = d.spec
	}
	return out
}

// Run executes a tool the model called with raw JSON input and returns
// the compact JSON result. Errors (bad input, budget, RPC failure) are
// returned so the caller can hand them back to the model as tool errors.
func (t *Toolbox) Run(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	def, ok := toolsByName[name]
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	in, err := def.decode(raw, t.req)
	if err != nil {
		t.recordInvalid(name, raw, err)
		return "", fmt.Errorf("invalid input for %s: %w", name, err)
	}
	out, err := t.invoke(ctx, name, in)
	if err != nil {
		return "", err
	}
	return compactJSON(out.compact), nil
}

// invoke runs a validated input, recording the step. The rules
// investigator calls it directly with typed inputs.
func (t *Toolbox) invoke(ctx context.Context, name string, in toolInput) (*toolOutput, error) {
	t.mu.Lock()
	if t.calls >= t.maxCalls {
		t.mu.Unlock()
		return nil, ErrBudget
	}
	t.calls++
	t.mu.Unlock()

	start := time.Now()
	out, err := toolsByName[name].run(ctx, t, in)
	inJSON, _ := json.Marshal(in)
	step := &kuberov1.InvestigateStep{
		Tool:       name,
		InputJson:  string(inJSON),
		DurationMs: float64(time.Since(start).Microseconds()) / 1000,
	}
	if err != nil {
		step.Error = true
		step.Summary = truncateRunes("error: "+err.Error(), 240)
	} else {
		step.Summary = out.summary
	}
	t.mu.Lock()
	t.steps = append(t.steps, step)
	t.mu.Unlock()
	t.emit.step(proto.Clone(step).(*kuberov1.InvestigateStep))
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (t *Toolbox) recordInvalid(name string, raw json.RawMessage, err error) {
	step := &kuberov1.InvestigateStep{
		Tool: name, InputJson: truncateRunes(string(raw), 1000), Error: true,
		Summary: truncateRunes("invalid input: "+err.Error(), 240),
	}
	t.mu.Lock()
	// Invalid calls still cost budget: a model looping on bad input must
	// run out of rope.
	t.calls++
	t.steps = append(t.steps, step)
	t.mu.Unlock()
	t.emit.step(proto.Clone(step).(*kuberov1.InvestigateStep))
}

// ─── tool plumbing ───────────────────────────────────────────────────────

type toolInput interface {
	// normalize validates and applies defaults (some depend on the request).
	normalize(req Request) error
}

type toolOutput struct {
	raw     any    // the proto response (the rules investigator reads it)
	compact any    // what the model sees
	summary string // one line for InvestigateStep.summary
}

type toolDef struct {
	spec   Spec
	decode func(json.RawMessage, Request) (toolInput, error)
	run    func(context.Context, *Toolbox, toolInput) (*toolOutput, error)
}

// strictDecode decodes into a fresh T, rejecting unknown fields and
// trailing data, then normalises it.
func strictDecode[T any, PT interface {
	*T
	toolInput
}](raw json.RawMessage, req Request) (toolInput, error) {
	var in T
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the input object")
	}
	p := PT(&in)
	if err := p.normalize(req); err != nil {
		return nil, err
	}
	return p, nil
}

var (
	toolDefs    []toolDef
	toolsByName = map[string]toolDef{}
)

func register(d toolDef) {
	toolDefs = append(toolDefs, d)
	toolsByName[d.spec.Name] = d
}

// ─── shared validation ───────────────────────────────────────────────────

var (
	allocWindows  = set("24h", "7d", "30d", "today", "yesterday", "week", "month", "lastmonth")
	allocDims     = set("cluster", "namespace", "workload", "controller", "node", "nodepool", "team", "cost_center", "zone")
	tsWindows     = set("24h", "7d", "30d")
	tsGroupBy     = set("", "namespace", "team", "cluster", "nodepool", "workload")
	anomalyWins   = set("24h", "7d", "30d")
	rsWindows     = set("7d", "14d", "30d")
	profileTypes  = set("cpu", "alloc_space", "alloc_objects", "inuse_space", "inuse_objects", "goroutine", "mutex", "block")
	alertStates   = set("", "pending", "firing", "resolved")
	sinceDuration = map[string]time.Duration{
		"5m": 5 * time.Minute, "15m": 15 * time.Minute, "1h": time.Hour,
		"6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
	}
	dnsLabelRE  = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	serviceRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,62}$`)
)

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func oneOf(field, v string, allowed map[string]bool) error {
	if !allowed[v] {
		return fmt.Errorf("%s %q must be one of: %s", field, v, strings.Join(keys(allowed), ", "))
	}
	return nil
}

func clampInt(v, def, lo, hi int) int {
	if v == 0 {
		return def
	}
	return max(lo, min(v, hi))
}

// defaultSince maps the request window onto a lookback.
func defaultSince(req Request) string {
	switch req.Window {
	case "1h":
		return "1h"
	case "7d":
		return "7d"
	}
	return "24h"
}

func validSince(s *string, req Request) error {
	if *s == "" {
		*s = defaultSince(req)
	}
	if _, ok := sinceDuration[*s]; !ok {
		return fmt.Errorf("since %q must be one of: 5m, 15m, 1h, 6h, 24h, 7d", *s)
	}
	return nil
}

func validNamespace(ns string) error {
	if ns != "" && !dnsLabelRE.MatchString(ns) {
		return fmt.Errorf("namespace %q is not a valid Kubernetes namespace name", ns)
	}
	return nil
}

func validFreeText(field, v string, maxLen int) error {
	if len(v) > maxLen {
		return fmt.Errorf("%s exceeds %d characters", field, maxLen)
	}
	if strings.ContainsFunc(v, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) {
		return fmt.Errorf("%s contains control characters", field)
	}
	return nil
}

func validLogQL(q string) error {
	q = strings.TrimSpace(q)
	if q == "" {
		return errors.New("query is required")
	}
	if err := validFreeText("query", q, maxQueryLen); err != nil {
		return err
	}
	if !strings.Contains(q, "{") || !strings.Contains(q, "}") {
		return errors.New(`query must contain a LogQL stream selector, e.g. {namespace="payments"}`)
	}
	return nil
}

func validFilters(f map[string]string) error {
	if len(f) > 4 {
		return errors.New("at most 4 filters")
	}
	for k, v := range f {
		if !allocDims[k] {
			return oneOf("filter key", k, allocDims)
		}
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("filter %s is empty", k)
		}
		if err := validFreeText("filter "+k, v, 128); err != nil {
			return err
		}
	}
	return nil
}

func (t *Toolbox) span(since string) (int64, int64) {
	d := sinceDuration[since]
	return t.now.Add(-d).UnixMilli(), t.now.UnixMilli()
}

// ─── tools ───────────────────────────────────────────────────────────────

type allocationIn struct {
	Window      string            `json:"window,omitempty"`
	Aggregate   []string          `json:"aggregate,omitempty"`
	Filters     map[string]string `json:"filters,omitempty"`
	IncludeIdle bool              `json:"include_idle,omitempty"`
	Limit       int               `json:"limit,omitempty"`
}

func (in *allocationIn) normalize(req Request) error {
	if in.Window == "" {
		in.Window = map[string]string{"1h": "24h", "24h": "24h", "7d": "7d"}[req.Window]
	}
	if err := oneOf("window", in.Window, allocWindows); err != nil {
		return err
	}
	if len(in.Aggregate) == 0 {
		in.Aggregate = []string{"namespace"}
	}
	if len(in.Aggregate) > 3 {
		return errors.New("aggregate takes at most 3 dimensions")
	}
	seen := map[string]bool{}
	for _, d := range in.Aggregate {
		if err := oneOf("aggregate", d, allocDims); err != nil {
			return err
		}
		if seen[d] {
			return fmt.Errorf("aggregate dimension %q repeated", d)
		}
		seen[d] = true
	}
	in.Limit = clampInt(in.Limit, 10, 1, 25)
	return validFilters(in.Filters)
}

type timeseriesIn struct {
	Window  string            `json:"window,omitempty"`
	Step    string            `json:"step,omitempty"`
	GroupBy string            `json:"group_by,omitempty"`
	Filters map[string]string `json:"filters,omitempty"`
	Top     int               `json:"top,omitempty"`
}

func (in *timeseriesIn) normalize(req Request) error {
	if in.Window == "" {
		in.Window = map[string]string{"1h": "24h", "24h": "7d", "7d": "30d"}[req.Window]
	}
	if err := oneOf("window", in.Window, tsWindows); err != nil {
		return err
	}
	if in.Step == "" {
		in.Step = "1h"
		if in.Window == "30d" {
			in.Step = "1d"
		}
	}
	if err := oneOf("step", in.Step, set("1h", "1d")); err != nil {
		return err
	}
	if in.GroupBy == "" {
		in.GroupBy = "namespace"
	}
	if err := oneOf("group_by", in.GroupBy, tsGroupBy); err != nil {
		return err
	}
	in.Top = clampInt(in.Top, 5, 1, 10)
	return validFilters(in.Filters)
}

type anomaliesIn struct {
	Window string `json:"window,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

func (in *anomaliesIn) normalize(req Request) error {
	if in.Window == "" {
		in.Window = map[string]string{"1h": "24h", "24h": "24h", "7d": "7d"}[req.Window]
	}
	in.Limit = clampInt(in.Limit, 10, 1, 20)
	return oneOf("window", in.Window, anomalyWins)
}

type rightsizingIn struct {
	Namespace  string  `json:"namespace,omitempty"`
	Window     string  `json:"window,omitempty"`
	MinSavings float64 `json:"min_savings_usd_month,omitempty"`
	Limit      int     `json:"limit,omitempty"`
}

func (in *rightsizingIn) normalize(Request) error {
	if in.Window == "" {
		in.Window = "7d"
	}
	if math.IsNaN(in.MinSavings) || in.MinSavings < 0 || in.MinSavings > 1e7 {
		return errors.New("min_savings_usd_month must be within 0..10,000,000")
	}
	in.Limit = clampInt(in.Limit, 10, 1, 20)
	if err := validNamespace(in.Namespace); err != nil {
		return err
	}
	return oneOf("window", in.Window, rsWindows)
}

type logQueryIn struct {
	Query string `json:"query"`
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (in *logQueryIn) normalize(req Request) error {
	in.Query = strings.TrimSpace(in.Query)
	in.Limit = clampInt(in.Limit, 30, 1, 100)
	if err := validLogQL(in.Query); err != nil {
		return err
	}
	return validSince(&in.Since, req)
}

type logPatternsIn struct {
	Query string `json:"query"`
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (in *logPatternsIn) normalize(req Request) error {
	in.Query = strings.TrimSpace(in.Query)
	in.Limit = clampInt(in.Limit, 10, 1, 20)
	if err := validLogQL(in.Query); err != nil {
		return err
	}
	return validSince(&in.Since, req)
}

type logVolumeIn struct {
	Query   string `json:"query"`
	Since   string `json:"since,omitempty"`
	GroupBy string `json:"group_by,omitempty"`
}

func (in *logVolumeIn) normalize(req Request) error {
	in.Query = strings.TrimSpace(in.Query)
	if in.GroupBy == "" {
		in.GroupBy = "level"
	}
	if !labelNameRE.MatchString(in.GroupBy) {
		return fmt.Errorf("group_by %q is not a label name", in.GroupBy)
	}
	if err := validLogQL(in.Query); err != nil {
		return err
	}
	return validSince(&in.Since, req)
}

type profileIn struct {
	Service   string `json:"service"`
	Namespace string `json:"namespace,omitempty"`
	Type      string `json:"type,omitempty"`
	Since     string `json:"since,omitempty"`
}

func (in *profileIn) normalizeProfile(req Request) error {
	if !serviceRE.MatchString(in.Service) {
		return errors.New("service is required (a workload / service name as listed by profile targets)")
	}
	if in.Type == "" {
		in.Type = "cpu"
	}
	if err := oneOf("type", in.Type, profileTypes); err != nil {
		return err
	}
	if err := validNamespace(in.Namespace); err != nil {
		return err
	}
	if in.Since == "" {
		// Profiles are dense; the last hour is plenty unless asked.
		in.Since = "1h"
		if req.Window == "7d" {
			in.Since = "24h"
		}
	}
	return validSince(&in.Since, req)
}

type topFunctionsIn struct {
	profileIn
	Limit   int    `json:"limit,omitempty"`
	OrderBy string `json:"order_by,omitempty"`
}

func (in *topFunctionsIn) normalize(req Request) error {
	in.Limit = clampInt(in.Limit, 10, 1, 25)
	if in.OrderBy == "" {
		in.OrderBy = "self"
	}
	if err := oneOf("order_by", in.OrderBy, set("self", "total")); err != nil {
		return err
	}
	return in.normalizeProfile(req)
}

type flamegraphIn struct {
	profileIn
	ComparePrevious bool `json:"compare_previous,omitempty"`
	MaxPaths        int  `json:"max_paths,omitempty"`
}

func (in *flamegraphIn) normalize(req Request) error {
	in.MaxPaths = clampInt(in.MaxPaths, 8, 1, 20)
	return in.normalizeProfile(req)
}

type serviceMapIn struct {
	Namespace string `json:"namespace,omitempty"`
	Since     string `json:"since,omitempty"`
	MaxEdges  int    `json:"max_edges,omitempty"`
}

func (in *serviceMapIn) normalize(req Request) error {
	in.MaxEdges = clampInt(in.MaxEdges, 10, 1, 30)
	if err := validNamespace(in.Namespace); err != nil {
		return err
	}
	return validSince(&in.Since, req)
}

type networkCostsIn struct {
	Since string `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (in *networkCostsIn) normalize(req Request) error {
	in.Limit = clampInt(in.Limit, 10, 1, 20)
	return validSince(&in.Since, req)
}

type alertsIn struct {
	State string `json:"state,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (in *alertsIn) normalize(Request) error {
	in.Limit = clampInt(in.Limit, 20, 1, 50)
	return oneOf("state", in.State, alertStates)
}

type capacityIn struct {
	Limit int `json:"limit,omitempty"`
}

func (in *capacityIn) normalize(Request) error {
	in.Limit = clampInt(in.Limit, 10, 1, 20)
	return nil
}

type workloadIn struct {
	Cluster   string `json:"cluster,omitempty"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (in *workloadIn) normalize(req Request) error {
	if in.Cluster == "" {
		in.Cluster = req.ClusterID
	}
	if in.Namespace == "" || in.Name == "" {
		return errors.New("namespace and name are required")
	}
	if err := validNamespace(in.Namespace); err != nil {
		return err
	}
	if !serviceRE.MatchString(in.Name) {
		return fmt.Errorf("name %q is not a workload name", in.Name)
	}
	return validFreeText("cluster", in.Cluster, MaxClusterIDLen)
}

// ─── schemas ─────────────────────────────────────────────────────────────

func obj(required []string, props map[string]any) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
}

func enumProp(desc string, vals ...string) map[string]any {
	return map[string]any{"type": "string", "enum": vals, "description": desc}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string, lo, hi int) map[string]any {
	return map[string]any{"type": "integer", "minimum": lo, "maximum": hi, "description": desc}
}

var (
	sinceProp   = enumProp("Lookback from now. Defaults to the investigation window.", "5m", "15m", "1h", "6h", "24h", "7d")
	filtersProp = map[string]any{
		"type":                 "object",
		"description":          "Exact-match filters, e.g. {\"namespace\": \"payments\"}. Keys: " + strings.Join(keys(allocDims), ", ") + ".",
		"additionalProperties": map[string]any{"type": "string"},
	}
	profileTypeProp = enumProp("Profile type (default cpu).", keys(profileTypes)...)
)

func init() {
	register(toolDef{
		spec: Spec{
			Name: "get_cost_allocation",
			Description: "Cost allocation (OpenCost-compatible) over a window, grouped by up to 3 dimensions, with CPU/RAM/GPU/network/idle cost, " +
				"usage efficiency (usage/request) and recoverable cost per row. Use it to find where money goes and who owns it.",
			Schema: obj(nil, map[string]any{
				"window":       enumProp("Allocation window (default follows the investigation window).", keys(allocWindows)...),
				"aggregate":    map[string]any{"type": "array", "items": enumProp("Dimension.", keys(allocDims)...), "description": "Group by, e.g. [\"namespace\"] or [\"namespace\",\"workload\"]."},
				"filters":      filtersProp,
				"include_idle": map[string]any{"type": "boolean", "description": "Add an __idle__ row per cluster."},
				"limit":        intProp("Max rows returned (default 10).", 1, 25),
			}),
		},
		decode: strictDecode[allocationIn],
		run:    runAllocation,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_cost_timeseries",
			Description: "Spend over time, optionally grouped. Each series reports recent_avg (last quarter of the window) vs baseline_avg (the rest) " +
				"and change_pct — the fastest way to find what jumped and when.",
			Schema: obj(nil, map[string]any{
				"window":   enumProp("Window (default: one step wider than the investigation window).", keys(tsWindows)...),
				"step":     enumProp("Resolution.", "1h", "1d"),
				"group_by": enumProp("Split series by this dimension (default namespace).", "namespace", "team", "cluster", "nodepool", "workload"),
				"filters":  filtersProp,
				"top":      intProp("Keep the N largest series (default 5).", 1, 10),
			}),
		},
		decode: strictDecode[timeseriesIn],
		run:    runTimeseries,
	})
	register(toolDef{
		spec: Spec{
			Name:        "list_anomalies",
			Description: "Statistically anomalous spend / capacity / posture signals, ranked by $/mo impact, each with a dashboard link.",
			Schema: obj(nil, map[string]any{
				"window": enumProp("Window.", keys(anomalyWins)...),
				"limit":  intProp("Max anomalies (default 10).", 1, 20),
			}),
		},
		decode: strictDecode[anomaliesIn],
		run:    runAnomalies,
	})
	register(toolDef{
		spec: Spec{
			Name: "list_rightsizing",
			Description: "Per-container rightsizing from measured usage percentiles: current vs recommended CPU/memory requests, $/mo savings " +
				"(negative = under-provisioned), confidence, OOM kills. Use for over-provisioning and memory-pressure questions.",
			Schema: obj(nil, map[string]any{
				"namespace":             strProp("Limit to one namespace."),
				"window":                enumProp("Observation window (default 7d).", keys(rsWindows)...),
				"min_savings_usd_month": map[string]any{"type": "number", "minimum": 0, "description": "Only rows saving at least this much."},
				"limit":                 intProp("Max rows (default 10).", 1, 20),
			}),
		},
		decode: strictDecode[rightsizingIn],
		run:    runRightsizing,
	})
	register(toolDef{
		spec: Spec{
			Name: "query_logs",
			Description: "Run a LogQL query. Log queries return lines (newest first; at most 50, bodies truncated to 300 chars), e.g. " +
				`{namespace="payments", level="error"} |= "timeout". Metric queries return series, e.g. ` +
				`sum by (namespace) (count_over_time({level="error"}[5m])). Stream labels: cluster, namespace, pod, container, workload, node, level, team.`,
			Schema: obj([]string{"query"}, map[string]any{
				"query": strProp("LogQL query (max 1000 chars)."),
				"since": sinceProp,
				"limit": intProp("Max lines for log queries (default 30).", 1, 100),
			}),
		},
		decode: strictDecode[logQueryIn],
		run:    runQueryLogs,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_log_patterns",
			Description: "Cluster matching log lines into templates (<_> = variable part) with counts, share, dominant level, a sample line and " +
				"a trend spike ratio (recent vs earlier). Turns a million lines into the dozen that matter.",
			Schema: obj([]string{"query"}, map[string]any{
				"query": strProp(`LogQL selector with optional filters, e.g. {namespace="payments"}.`),
				"since": sinceProp,
				"limit": intProp("Max patterns (default 10).", 1, 20),
			}),
		},
		decode: strictDecode[logPatternsIn],
		run:    runLogPatterns,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_log_volume",
			Description: "Log line volume for a selector split by a label (default level), with totals, a spike ratio per series and what the " +
				"volume costs per month to store. Use it to spot error spikes and noisy log sources.",
			Schema: obj([]string{"query"}, map[string]any{
				"query":    strProp(`LogQL selector, e.g. {level="error"}.`),
				"since":    sinceProp,
				"group_by": strProp("Label to split by (default level), e.g. namespace or workload."),
			}),
		},
		decode: strictDecode[logVolumeIn],
		run:    runLogVolume,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_top_functions",
			Description: "Continuous-profiling top functions for a service (eBPF / pprof), ranked by self or total time, with the $/mo each " +
				"function's CPU costs. Use for slow / CPU-hungry services.",
			Schema: obj([]string{"service"}, map[string]any{
				"service":   strProp("Service / workload name."),
				"namespace": strProp("Namespace, when the name is ambiguous."),
				"type":      profileTypeProp,
				"since":     sinceProp,
				"limit":     intProp("Max functions (default 10).", 1, 25),
				"order_by":  enumProp("Rank by self (default) or total time.", "self", "total"),
			}),
		},
		decode: strictDecode[topFunctionsIn],
		run:    runTopFunctions,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_flamegraph_summary",
			Description: "The hottest call paths (root → leaf) of a service's flamegraph with self/total share. compare_previous=true diffs " +
				"against the preceding window of equal length and reports delta_pct per path — regressions first.",
			Schema: obj([]string{"service"}, map[string]any{
				"service":          strProp("Service / workload name."),
				"namespace":        strProp("Namespace, when the name is ambiguous."),
				"type":             profileTypeProp,
				"since":            sinceProp,
				"compare_previous": map[string]any{"type": "boolean", "description": "Diff against the previous window."},
				"max_paths":        intProp("Max paths (default 8).", 1, 20),
			}),
		},
		decode: strictDecode[flamegraphIn],
		run:    runFlamegraph,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_service_map",
			Description: "eBPF service map edges (who talks to whom) ranked by $/mo: bytes, cross-zone and internet-egress flags, TCP " +
				"retransmits. Use for network cost and connectivity questions.",
			Schema: obj(nil, map[string]any{
				"namespace": strProp("Focus namespace."),
				"since":     sinceProp,
				"max_edges": intProp("Max edges (default 10).", 1, 30),
			}),
		},
		decode: strictDecode[serviceMapIn],
		run:    runServiceMap,
	})
	register(toolDef{
		spec: Spec{
			Name:        "list_network_costs",
			Description: "Per-workload network spend: internet egress and cross-zone transfer ($/mo and GB), with each workload's top destination.",
			Schema: obj(nil, map[string]any{
				"since": sinceProp,
				"limit": intProp("Max rows (default 10).", 1, 20),
			}),
		},
		decode: strictDecode[networkCostsIn],
		run:    runNetworkCosts,
	})
	register(toolDef{
		spec: Spec{
			Name:        "list_alerts",
			Description: "Alerts from KubeHero's alerting engine (logs, cost, budget, anomaly, network, event rules): state, severity, value, labels, timing.",
			Schema: obj(nil, map[string]any{
				"state": enumProp("Filter by state (default: all active + recently resolved).", "pending", "firing", "resolved"),
				"limit": intProp("Max alerts (default 20).", 1, 50),
			}),
		},
		decode: strictDecode[alertsIn],
		run:    runAlerts,
	})
	register(toolDef{
		spec: Spec{
			Name: "list_capacity_demands",
			Description: "Pods that cannot schedule on current capacity (pending demand), with the capacity change that would unblock them " +
				"and what doing nothing costs. The closest thing to cluster events for scheduling problems.",
			Schema: obj(nil, map[string]any{
				"limit": intProp("Max rows (default 10).", 1, 20),
			}),
		},
		decode: strictDecode[capacityIn],
		run:    runCapacity,
	})
	register(toolDef{
		spec: Spec{
			Name: "get_workload",
			Description: "One workload's latest waste recommendation plus its audit history (policy actions, scaling and config changes " +
				"recorded by KubeHero), newest first. Use to correlate a change with a symptom.",
			Schema: obj([]string{"namespace", "name"}, map[string]any{
				"cluster":   strProp("Cluster id or name (default: the investigation's cluster)."),
				"namespace": strProp("Namespace."),
				"name":      strProp("Workload name."),
			}),
		},
		decode: strictDecode[workloadIn],
		run:    runWorkload,
	})
}

// ─── runners ─────────────────────────────────────────────────────────────

func runAllocation(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*allocationIn)
	res, err := t.backend.GetAllocation(ctx, &kuberov1.GetAllocationRequest{
		Window: in.Window, Aggregate: in.Aggregate, Filters: in.Filters, IncludeIdle: in.IncludeIdle, ClusterId: t.req.ClusterID,
	})
	if err != nil {
		return nil, err
	}
	allocs := append([]*kuberov1.Allocation(nil), res.GetAllocations()...)
	sort.SliceStable(allocs, func(i, j int) bool { return allocs[i].GetTotalCost() > allocs[j].GetTotalCost() })
	rows := make([]map[string]any, 0, in.Limit)
	for _, a := range allocs {
		if len(rows) >= in.Limit {
			break
		}
		rows = append(rows, map[string]any{
			"name": a.GetName(), "total_cost": r2(a.GetTotalCost()), "cpu_cost": r2(a.GetCpuCost()),
			"ram_cost": r2(a.GetRamCost()), "gpu_cost": r2(a.GetGpuCost()), "network_cost": r2(a.GetNetworkCost()),
			"idle_cost": r2(a.GetIdleCost()), "cpu_efficiency": r2(a.GetCpuEfficiency()), "ram_efficiency": r2(a.GetRamEfficiency()),
			"recoverable_cost": r2(a.GetRecoverableCost()),
		})
	}
	summary := fmt.Sprintf("%d rows by %s over %s", len(allocs), strings.Join(in.Aggregate, "+"), in.Window)
	if len(allocs) > 0 {
		summary += fmt.Sprintf(" · top %s %s", allocs[0].GetName(), usd(allocs[0].GetTotalCost()))
	}
	return &toolOutput{
		raw: res,
		compact: map[string]any{
			"window": in.Window, "source": res.GetSource(), "total_cost": r2(res.GetTotals().GetTotalCost()),
			"rows": rows, "rows_total": len(allocs),
		},
		summary: summary,
	}, nil
}

// SeriesChange is a series' labels plus its strongest recent shift.
type SeriesChange struct {
	Labels map[string]string
	source.Shift
}

func seriesChange(s *kuberov1.Series) SeriesChange {
	return SeriesChange{Labels: s.GetLabels(), Shift: source.SeriesShift(s.GetPoints())}
}

func runTimeseries(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*timeseriesIn)
	res, err := t.backend.GetCostTimeseries(ctx, &kuberov1.GetCostTimeseriesRequest{
		Window: in.Window, Step: in.Step, GroupBy: in.GroupBy, Filters: in.Filters, Top: int32(in.Top), ClusterId: t.req.ClusterID,
	})
	if err != nil {
		return nil, err
	}
	series := make([]map[string]any, 0, len(res.GetSeries()))
	var mover SeriesChange
	for i, s := range res.GetSeries() {
		if i >= in.Top {
			break
		}
		c := seriesChange(s)
		if math.Abs(c.ChangePct) > math.Abs(mover.ChangePct) {
			mover = c
		}
		series = append(series, map[string]any{
			"labels": s.GetLabels(), "total": r2(c.Total), "recent_avg": r2(c.Recent),
			"baseline_avg": r2(c.Baseline), "change_pct": r2(c.ChangePct), "points": downsample(s.GetPoints(), 24),
		})
	}
	summary := fmt.Sprintf("%d series over %s, total %s", len(series), in.Window, usd(res.GetTotalUsd()))
	if mover.Labels != nil && mover.ChangePct != 0 {
		summary += fmt.Sprintf(" · biggest mover %s %+.0f%%", labelSummary(mover.Labels), mover.ChangePct)
	}
	return &toolOutput{
		raw: res,
		compact: map[string]any{
			"window": in.Window, "step": in.Step, "total_usd": r2(res.GetTotalUsd()),
			"forecast_month_usd": r2(res.GetForecastMonthUsd()), "source": res.GetSource(), "series": series,
		},
		summary: summary,
	}, nil
}

func runAnomalies(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*anomaliesIn)
	res, err := t.backend.ListAnomalies(ctx, &kuberov1.ListAnomaliesRequest{Window: in.Window, Limit: int32(in.Limit)})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, a := range capSlice(res.GetAnomalies(), in.Limit) {
		rows = append(rows, map[string]any{
			"id": a.GetId(), "kind": a.GetKind(), "title": a.GetTitle(), "subject": a.GetSubject(),
			"detail": truncateRunes(a.GetDetail(), maxLineRunes), "delta_pct": r2(a.GetDeltaPct()),
			"impact_usd_month": r2(a.GetImpactUsdMonth()), "severity": a.GetSeverity(), "link_path": a.GetLinkPath(),
		})
	}
	summary := fmt.Sprintf("%d anomalies", len(res.GetAnomalies()))
	if len(res.GetAnomalies()) > 0 {
		summary += " · top: " + res.GetAnomalies()[0].GetTitle()
	}
	return &toolOutput{raw: res, compact: map[string]any{"anomalies": rows}, summary: summary}, nil
}

func runRightsizing(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*rightsizingIn)
	res, err := t.backend.ListRightsizing(ctx, &kuberov1.ListRightsizingRequest{
		ClusterId: t.req.ClusterID, Namespace: in.Namespace, Window: in.Window,
		MinSavingsUsdMonth: in.MinSavings, Limit: int32(in.Limit),
	})
	if err != nil {
		return nil, err
	}
	const gib = float64(1 << 30)
	rows := []map[string]any{}
	oom := 0
	for _, r := range capSlice(res.GetRecommendations(), in.Limit) {
		if r.GetOomKills() > 0 {
			oom++
		}
		rows = append(rows, map[string]any{
			"namespace": r.GetNamespace(), "workload": r.GetWorkload(), "kind": r.GetWorkloadKind(), "container": r.GetContainer(),
			"replicas": r.GetReplicas(), "cpu_request_cores": r2(r.GetCpuRequestCores()), "cpu_p95_cores": r2(r.GetCpuP95Cores()),
			"cpu_recommended_cores": r2(r.GetCpuRecommendedCores()), "mem_request_gib": r2(float64(r.GetMemRequestBytes()) / gib),
			"mem_max_gib": r2(float64(r.GetMemMaxBytes()) / gib), "mem_recommended_gib": r2(float64(r.GetMemRecommendedBytes()) / gib),
			"savings_usd_month": r2(r.GetSavingsUsdMonth()), "confidence": r.GetConfidence(), "direction": r.GetDirection(),
			"oom_kills": r.GetOomKills(), "reason": truncateRunes(r.GetReason(), maxLineRunes),
		})
	}
	summary := fmt.Sprintf("%d recommendations, %s/mo total", len(res.GetRecommendations()), usd(res.GetTotalSavingsUsdMonth()))
	if oom > 0 {
		summary += fmt.Sprintf(" · %d with OOM kills", oom)
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"source": res.GetSource(), "total_savings_usd_month": r2(res.GetTotalSavingsUsdMonth()), "recommendations": rows},
		summary: summary,
	}, nil
}

func runQueryLogs(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*logQueryIn)
	start, end := t.span(in.Since)
	res, err := t.backend.QueryLogs(ctx, &kuberov1.QueryLogsRequest{
		Query: in.Query, StartUnixMs: start, EndUnixMs: end, Limit: int32(in.Limit), Direction: "backward", ClusterId: t.req.ClusterID,
	})
	if err != nil {
		return nil, err
	}
	if res.GetResultType() == "matrix" {
		series := seriesDigest(res.GetSeries(), 10)
		return &toolOutput{
			raw:     res,
			compact: map[string]any{"result_type": "matrix", "series": series},
			summary: fmt.Sprintf("%d series", len(res.GetSeries())),
		}, nil
	}
	lines := []map[string]any{}
	for _, l := range capSlice(res.GetLines(), maxLogLines) {
		lab := l.GetLabels()
		lines = append(lines, map[string]any{
			"ts": time.Unix(0, l.GetTsUnixNano()).UTC().Format(time.RFC3339), "level": l.GetLevel(),
			"namespace": lab["namespace"], "workload": lab["workload"], "pod": lab["pod"],
			"body": truncateRunes(l.GetBody(), maxLineRunes),
		})
	}
	summary := fmt.Sprintf("%d lines", len(res.GetLines()))
	if len(res.GetLines()) > 0 {
		summary += " · newest: " + truncateRunes(res.GetLines()[0].GetBody(), 80)
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"result_type": "streams", "lines": lines, "returned": len(res.GetLines())},
		summary: summary,
	}, nil
}

func runLogPatterns(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*logPatternsIn)
	start, end := t.span(in.Since)
	res, err := t.backend.GetLogPatterns(ctx, &kuberov1.GetLogPatternsRequest{
		Query: in.Query, StartUnixMs: start, EndUnixMs: end, Limit: int32(in.Limit), ClusterId: t.req.ClusterID,
	})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, p := range capSlice(res.GetPatterns(), in.Limit) {
		_, spike := source.SeriesSpike(p.GetTrend())
		rows = append(rows, map[string]any{
			"pattern": truncateRunes(p.GetPattern(), maxLineRunes), "count": p.GetCount(), "level": p.GetLevel(),
			"share_pct": r2(p.GetSharePct()), "sample": truncateRunes(p.GetSample(), maxLineRunes), "trend_spike_ratio": spike,
		})
	}
	summary := fmt.Sprintf("%d patterns over %d lines", len(res.GetPatterns()), res.GetLinesAnalyzed())
	if len(res.GetPatterns()) > 0 {
		summary += fmt.Sprintf(" · top %q ×%d", truncateRunes(res.GetPatterns()[0].GetPattern(), 60), res.GetPatterns()[0].GetCount())
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"lines_analyzed": res.GetLinesAnalyzed(), "patterns": rows},
		summary: summary,
	}, nil
}

func runLogVolume(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*logVolumeIn)
	start, end := t.span(in.Since)
	res, err := t.backend.GetLogVolume(ctx, &kuberov1.GetLogVolumeRequest{
		Query: in.Query, StartUnixMs: start, EndUnixMs: end, GroupBy: in.GroupBy, ClusterId: t.req.ClusterID,
	})
	if err != nil {
		return nil, err
	}
	series := seriesDigest(res.GetSeries(), 10)
	summary := fmt.Sprintf("%d lines across %d series", res.GetTotalLines(), len(res.GetSeries()))
	if len(series) > 0 {
		summary += fmt.Sprintf(" · largest %s (spike %.1fx)", labelSummary(series[0]["labels"].(map[string]string)), series[0]["spike_ratio"])
	}
	return &toolOutput{
		raw: res,
		compact: map[string]any{
			"total_lines": res.GetTotalLines(), "total_bytes": res.GetTotalBytes(),
			"est_cost_usd_month": r2(res.GetEstCostUsdMonth()), "series": series,
		},
		summary: summary,
	}, nil
}

func (in *profileIn) selector(clusterID string) *kuberov1.ProfileSelector {
	return &kuberov1.ProfileSelector{Service: in.Service, Type: in.Type, Namespace: in.Namespace, ClusterId: clusterID}
}

func runTopFunctions(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*topFunctionsIn)
	start, end := t.span(in.Since)
	res, err := t.backend.GetTopFunctions(ctx, &kuberov1.GetTopFunctionsRequest{
		Selector: in.selector(t.req.ClusterID), StartUnixMs: start, EndUnixMs: end, Limit: int32(in.Limit), OrderBy: in.OrderBy,
	})
	if err != nil {
		return nil, err
	}
	fns := []map[string]any{}
	for _, f := range capSlice(res.GetFunctions(), in.Limit) {
		fns = append(fns, map[string]any{
			"name": truncateRunes(f.GetName(), 200), "self_pct": r2(f.GetSelfPct()), "total_pct": r2(f.GetTotalPct()),
			"self_cost_usd_month": r2(f.GetSelfCostUsdMonth()),
		})
	}
	summary := fmt.Sprintf("%d functions for %s (%s)", len(res.GetFunctions()), in.Service, in.Type)
	if len(res.GetFunctions()) > 0 {
		f := res.GetFunctions()[0]
		summary += fmt.Sprintf(" · top %s %.0f%% self", truncateRunes(f.GetName(), 60), f.GetSelfPct())
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"service": in.Service, "type": in.Type, "unit": res.GetUnit(), "total": res.GetTotal(), "functions": fns},
		summary: summary,
	}, nil
}

// HotPath is one root→leaf path of a flamegraph.
type HotPath struct {
	Path     string
	SelfPct  float64
	TotalPct float64
	DeltaPct float64 // diff mode: self share change vs the baseline, in points
}

// hotPaths ranks leaf frames by self time (or by regression in diff
// mode) and renders each as "root > ... > leaf".
func hotPaths(res *kuberov1.GetFlamegraphResponse, diff bool, n int) []HotPath {
	nodes := res.GetNodes()
	total := float64(res.GetTotal())
	baseTotal := float64(res.GetBaselineTotal())
	if total <= 0 {
		return nil
	}
	var out []HotPath
	for i, node := range nodes {
		if i == 0 || node.GetSelf() <= 0 {
			continue
		}
		var frames []string
		for j := i; j > 0 && j < len(nodes); j = int(nodes[j].GetParent()) {
			frames = append([]string{truncateRunes(nodes[j].GetName(), 80)}, frames...)
			if len(frames) > 12 {
				frames = append([]string{"…"}, frames[len(frames)-11:]...)
				break
			}
		}
		hp := HotPath{
			Path:     truncateRunes(strings.Join(frames, " > "), 600),
			SelfPct:  100 * float64(node.GetSelf()) / total,
			TotalPct: 100 * float64(node.GetTotal()) / total,
		}
		if diff && baseTotal > 0 {
			hp.DeltaPct = hp.SelfPct - 100*float64(node.GetBaselineSelf())/baseTotal
		}
		out = append(out, hp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if diff {
			return out[i].DeltaPct > out[j].DeltaPct
		}
		return out[i].SelfPct > out[j].SelfPct
	})
	return capSlice(out, n)
}

func runFlamegraph(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*flamegraphIn)
	start, end := t.span(in.Since)
	req := &kuberov1.GetFlamegraphRequest{Selector: in.selector(t.req.ClusterID), StartUnixMs: start, EndUnixMs: end, MaxNodes: 2048}
	if in.ComparePrevious {
		req.BaselineStartUnixMs = start - (end - start)
		req.BaselineEndUnixMs = start
	}
	res, err := t.backend.GetFlamegraph(ctx, req)
	if err != nil {
		return nil, err
	}
	diff := in.ComparePrevious && res.GetBaselineTotal() > 0
	paths := []map[string]any{}
	hps := hotPaths(res, diff, in.MaxPaths)
	for _, hp := range hps {
		row := map[string]any{"path": hp.Path, "self_pct": r2(hp.SelfPct), "total_pct": r2(hp.TotalPct)}
		if diff {
			row["delta_pct"] = r2(hp.DeltaPct)
		}
		paths = append(paths, row)
	}
	summary := fmt.Sprintf("%d frames for %s (%s)", len(res.GetNodes()), in.Service, in.Type)
	if len(hps) > 0 {
		leaf := hps[0].Path[strings.LastIndex(hps[0].Path, " > ")+len(" > "):]
		if diff {
			summary += fmt.Sprintf(" · biggest regression %s %+.1fpp", truncateRunes(leaf, 60), hps[0].DeltaPct)
		} else {
			summary += fmt.Sprintf(" · hottest %s %.0f%%", truncateRunes(leaf, 60), hps[0].SelfPct)
		}
	}
	return &toolOutput{
		raw: res,
		compact: map[string]any{
			"service": in.Service, "type": res.GetType(), "unit": res.GetUnit(), "total": res.GetTotal(),
			"cost_usd_month": r2(res.GetCostUsdMonth()), "diff": diff, "hot_paths": paths,
		},
		summary: summary,
	}, nil
}

func runServiceMap(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*serviceMapIn)
	start, end := t.span(in.Since)
	res, err := t.backend.GetServiceMap(ctx, &kuberov1.GetServiceMapRequest{
		ClusterId: t.req.ClusterID, Namespace: in.Namespace, StartUnixMs: start, EndUnixMs: end, MaxNodes: 60,
	})
	if err != nil {
		return nil, err
	}
	edges := append([]*kuberov1.ServiceMapEdge(nil), res.GetEdges()...)
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].GetCostUsdMonth() != edges[j].GetCostUsdMonth() {
			return edges[i].GetCostUsdMonth() > edges[j].GetCostUsdMonth()
		}
		return edges[i].GetBytes() > edges[j].GetBytes()
	})
	rows := []map[string]any{}
	for _, e := range capSlice(edges, in.MaxEdges) {
		rows = append(rows, map[string]any{
			"source": e.GetSource(), "target": e.GetTarget(), "port": e.GetPort(), "protocol": e.GetProtocol(),
			"gb": r2(e.GetBytes() / 1e9), "cost_usd_month": r2(e.GetCostUsdMonth()), "cross_zone": e.GetCrossZone(),
			"egress": e.GetEgress(), "retransmits": e.GetRetransmits(),
		})
	}
	summary := fmt.Sprintf("%d edges, %s/mo", len(res.GetEdges()), usd(res.GetTotalCostUsdMonth()))
	if len(edges) > 0 {
		summary += fmt.Sprintf(" · top %s → %s %s/mo", edges[0].GetSource(), edges[0].GetTarget(), usd(edges[0].GetCostUsdMonth()))
	}
	return &toolOutput{
		raw: res,
		compact: map[string]any{
			"total_cost_usd_month": r2(res.GetTotalCostUsdMonth()), "egress_gb": r2(res.GetEgressGb()),
			"cross_zone_gb": r2(res.GetCrossZoneGb()), "source": res.GetSource(), "edges": rows,
		},
		summary: summary,
	}, nil
}

func runNetworkCosts(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*networkCostsIn)
	start, end := t.span(in.Since)
	res, err := t.backend.ListNetworkCosts(ctx, &kuberov1.ListNetworkCostsRequest{
		ClusterId: t.req.ClusterID, StartUnixMs: start, EndUnixMs: end, Limit: int32(in.Limit),
	})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, c := range capSlice(res.GetCosts(), in.Limit) {
		rows = append(rows, map[string]any{
			"namespace": c.GetNamespace(), "workload": c.GetWorkload(), "egress_gb": r2(c.GetEgressGb()),
			"cross_zone_gb": r2(c.GetCrossZoneGb()), "egress_usd_month": r2(c.GetEgressUsdMonth()),
			"cross_zone_usd_month": r2(c.GetCrossZoneUsdMonth()), "total_usd_month": r2(c.GetTotalUsdMonth()),
			"top_destination": c.GetTopDestination(),
		})
	}
	summary := fmt.Sprintf("%d workloads, %s/mo", len(res.GetCosts()), usd(res.GetTotalUsdMonth()))
	if len(res.GetCosts()) > 0 {
		c := res.GetCosts()[0]
		summary += fmt.Sprintf(" · top %s/%s %s/mo", c.GetNamespace(), c.GetWorkload(), usd(c.GetTotalUsdMonth()))
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"total_usd_month": r2(res.GetTotalUsdMonth()), "source": res.GetSource(), "costs": rows},
		summary: summary,
	}, nil
}

func runAlerts(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*alertsIn)
	res, err := t.backend.ListAlerts(ctx, &kuberov1.ListAlertsRequest{State: in.State, Limit: int32(in.Limit)})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, a := range capSlice(res.GetAlerts(), in.Limit) {
		rows = append(rows, map[string]any{
			"name": a.GetRuleName(), "kind": a.GetKind(), "state": a.GetState(), "severity": a.GetSeverity(),
			"summary": truncateRunes(a.GetSummary(), maxLineRunes), "value": r2(a.GetValue()), "labels": a.GetLabels(),
			"started_at": a.GetStartedAt(), "fired_at": a.GetFiredAt(), "resolved_at": a.GetResolvedAt(),
			"silenced": a.GetSilenced(), "link_path": a.GetLinkPath(),
		})
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"firing": res.GetFiring(), "pending": res.GetPending(), "alerts": rows},
		summary: fmt.Sprintf("%d alerts (%d firing, %d pending)", len(res.GetAlerts()), res.GetFiring(), res.GetPending()),
	}, nil
}

func runCapacity(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*capacityIn)
	res, err := t.backend.ListCapacityDemands(ctx, &kuberov1.ListCapacityDemandsRequest{ClusterId: t.req.ClusterID, Limit: int32(in.Limit)})
	if err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for _, d := range capSlice(res.GetDemands(), in.Limit) {
		rows = append(rows, map[string]any{
			"cluster": d.GetCluster(), "namespace": d.GetNamespace(), "workload": d.GetWorkload(),
			"pending_pods": d.GetPendingPods(), "requested_cpu": d.GetRequestedCpu(), "requested_mem": d.GetRequestedMem(),
			"requested_gpu": d.GetRequestedGpu(), "oldest_pending_age": d.GetOldestPendingAge(),
			"recommended_action": d.GetRecommendedAction(), "recommended_cost_usd_month": r2(d.GetRecommendedCostUsdMonth()),
			"blocked_cost_usd_month": r2(d.GetBlockedCostUsdMonth()),
		})
	}
	return &toolOutput{
		raw:     res,
		compact: map[string]any{"total_pending_pods": res.GetTotalPendingPods(), "demands": rows},
		summary: fmt.Sprintf("%d demands, %d pending pods", len(res.GetDemands()), res.GetTotalPendingPods()),
	}, nil
}

func runWorkload(ctx context.Context, t *Toolbox, ti toolInput) (*toolOutput, error) {
	in := ti.(*workloadIn)
	res, err := t.backend.GetWorkload(ctx, &kuberov1.GetWorkloadRequest{Cluster: in.Cluster, Namespace: in.Namespace, Name: in.Name})
	if err != nil {
		return nil, err
	}
	out := map[string]any{"namespace": in.Namespace, "name": in.Name}
	if rec := res.GetRecommendation(); rec != nil {
		out["recommendation"] = map[string]any{
			"signal": rec.GetSignal(), "recoverable_usd_month": r2(rec.GetRecoverableUsdMonth()),
			"action": rec.GetAction(), "severity": rec.GetSeverity(),
		}
	}
	hist := []map[string]any{}
	for _, h := range capSlice(res.GetHistory(), 10) {
		hist = append(hist, map[string]any{
			"id": h.GetId(), "at": h.GetAt(), "policy": h.GetPolicy(), "action": truncateRunes(h.GetAction(), 200),
			"outcome": h.GetOutcome(),
		})
	}
	out["history"] = hist
	return &toolOutput{
		raw: res, compact: out,
		summary: fmt.Sprintf("%s/%s: %d audit entries", in.Namespace, in.Name, len(res.GetHistory())),
	}, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────

func seriesDigest(series []*kuberov1.Series, n int) []map[string]any {
	type dig struct {
		labels      map[string]string
		total, last float64
		maxV, spike float64
	}
	ds := make([]dig, 0, len(series))
	for _, s := range series {
		d := dig{labels: s.GetLabels()}
		for _, p := range s.GetPoints() {
			d.total += p.GetValue()
			d.last = p.GetValue()
			d.maxV = math.Max(d.maxV, p.GetValue())
		}
		_, d.spike = source.SeriesSpike(s.GetPoints())
		ds = append(ds, d)
	}
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].total > ds[j].total })
	out := []map[string]any{}
	for _, d := range capSlice(ds, n) {
		labels := d.labels
		if labels == nil {
			labels = map[string]string{}
		}
		out = append(out, map[string]any{"labels": labels, "total": r2(d.total), "last": r2(d.last), "max": r2(d.maxV), "spike_ratio": d.spike})
	}
	return out
}

func downsample(pts []*kuberov1.Point, n int) [][2]any {
	if len(pts) == 0 {
		return [][2]any{}
	}
	stride := (len(pts) + n - 1) / n
	out := make([][2]any, 0, n)
	for i := 0; i < len(pts); i += stride {
		end := min(i+stride, len(pts))
		sum := 0.0
		for _, p := range pts[i:end] {
			sum += p.GetValue()
		}
		out = append(out, [2]any{time.UnixMilli(pts[i].GetTsUnixMs()).UTC().Format(time.RFC3339), r2(sum)})
	}
	return out
}

func labelSummary(labels map[string]string) string {
	if len(labels) == 0 {
		return "total"
	}
	ks := make([]string, 0, len(labels))
	for k := range labels {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	parts := make([]string, 0, len(ks))
	for _, k := range ks {
		parts = append(parts, labels[k])
	}
	return strings.Join(parts, "/")
}

// compactJSON marshals a tool result, falling back to a clearly marked
// partial rendering when it exceeds maxResultBytes.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return `{"error":"result could not be encoded"}`
	}
	if len(b) <= maxResultBytes {
		return string(b)
	}
	// Escaping inflates the embedded text, so shrink until it fits.
	for budget := maxResultBytes - 300; budget > 0; budget = budget * 3 / 4 {
		partial, _ := json.Marshal(map[string]any{
			"truncated": true,
			"note":      "result exceeded the size budget; narrow the query (filters, limit, shorter window) for detail",
			"partial":   truncateBytes(string(b), budget),
		})
		if len(partial) <= maxResultBytes {
			return string(partial)
		}
	}
	return `{"truncated":true}`
}

// truncateBytes cuts s to at most n bytes without splitting a rune.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func capSlice[T any](in []T, n int) []T {
	if n >= 0 && len(in) > n {
		return in[:n]
	}
	return in
}

func r2(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*100) / 100
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// usd renders dollars for summaries: "$1,234" (or "$0.42" below $1).
func usd(v float64) string {
	if v == 0 {
		return "$0"
	}
	if math.Abs(v) < 1 {
		return fmt.Sprintf("$%.2f", v)
	}
	neg := v < 0
	s := fmt.Sprintf("%.0f", math.Abs(v))
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-$" + b.String()
	}
	return "$" + b.String()
}
