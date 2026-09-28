// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/backend"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/investigate"
)

// DefaultInvestigationTimeout bounds one LLM investigation end to end.
const DefaultInvestigationTimeout = 120 * time.Second

var evidenceSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"kind": map[string]any{"type": "string", "enum": []string{
			"cost", "anomaly", "logs", "profile", "network", "event", "rightsizing", "alert"}},
		"title":     map[string]any{"type": "string"},
		"detail":    map[string]any{"type": "string"},
		"link_path": map[string]any{"type": "string", "description": `dashboard path starting with "/", or ""`},
		"query":     map[string]any{"type": "string", "description": `query that produced it, or ""`},
	},
	"required":             []string{"kind", "title", "detail", "link_path", "query"},
	"additionalProperties": false,
}

var investigationSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"answer_markdown": map[string]any{"type": "string"},
		"spoken_summary":  map[string]any{"type": "string"},
		"evidence":        map[string]any{"type": "array", "items": evidenceSchema},
		"actions":         map[string]any{"type": "array", "items": actionSchema},
	},
	"required":             []string{"answer_markdown", "spoken_summary", "evidence", "actions"},
	"additionalProperties": false,
}

// Investigator runs an agentic, read-only tool loop with Claude and
// falls back to the rules investigator on any failure or refusal.
type Investigator struct {
	client   anthropic.Client
	cfg      Config
	backend  backend.Backend
	fallback investigate.Investigator
	log      *slog.Logger

	// Now, MaxCalls and Timeout are injectable for tests.
	Now      func() time.Time
	MaxCalls int
	Timeout  time.Duration
}

// NewInvestigator wires the LLM investigator.
func NewInvestigator(b backend.Backend, fallback investigate.Investigator, log *slog.Logger, cfg Config) *Investigator {
	return &Investigator{client: cfg.client(), cfg: cfg, backend: b, fallback: fallback, log: log}
}

func (inv *Investigator) Investigate(ctx context.Context, req investigate.Request, emit investigate.Emitter) (*kuberov1.InvestigateResponse, error) {
	res, err := inv.run(ctx, req, emit)
	if err == nil {
		return res, nil
	}
	attrs := []any{"err", err}
	if r, ok := IsRefusal(err); ok {
		attrs = append(attrs, "refusal_category", r.Category)
	}
	inv.log.Warn("llm investigation failed, falling back to rules", attrs...)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	emit.Progress("The AI investigator couldn't finish; answering from KubeHero's deterministic rules instead")
	return inv.fallback.Investigate(ctx, req, emit)
}

func (inv *Investigator) run(ctx context.Context, req investigate.Request, emit investigate.Emitter) (*kuberov1.InvestigateResponse, error) {
	timeout := inv.Timeout
	if timeout <= 0 {
		timeout = DefaultInvestigationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	now := time.Now()
	if inv.Now != nil {
		now = inv.Now()
	}
	maxCalls := inv.MaxCalls
	if maxCalls <= 0 {
		maxCalls = investigate.DefaultMaxCalls
	}
	tb := investigate.NewToolbox(inv.backend, req, now, maxCalls, emit)

	var tools []anthropic.BetaTool
	for _, spec := range investigate.Specs() {
		tools = append(tools, betaTool(tb, spec))
	}

	params := inv.cfg.baseParams(investigateSystemPrompt)
	params.Messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(userPrompt(req, now, inv.backend.Origin()))),
	}
	params.OutputConfig = anthropic.BetaOutputConfigParam{
		Format: anthropic.BetaJSONOutputFormatParam{Schema: investigationSchema},
	}

	emit.Progress("Planning the investigation")
	runner := inv.client.Beta.Messages.NewToolRunnerStreaming(tools, anthropic.BetaToolRunnerParams{
		BetaMessageNewParams: params,
		// Turns, not tool calls (one turn may call several tools); the
		// toolbox enforces the call budget itself.
		MaxIterations: maxCalls + 3,
	})
	for turn, err := range runner.AllStreaming(ctx) {
		if err != nil {
			return nil, err
		}
		for ev, err := range turn {
			if err != nil {
				return nil, err
			}
			if ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use" {
				emit.Progress("Calling " + ev.ContentBlock.Name)
			}
		}
	}
	if err := runner.Err(); err != nil {
		return nil, err
	}
	msg := runner.LastMessage()
	if msg == nil {
		return nil, errors.New("no response from the model")
	}
	if err := checkStop(msg); err != nil {
		return nil, err
	}
	if model, fell := servedBy(msg); fell {
		inv.log.Info("investigation served by server-side fallback model", "model", model)
	}
	res, err := parseInvestigation(messageText(msg))
	if err != nil {
		return nil, fmt.Errorf("parse llm output: %w", err)
	}
	res.Steps = tb.Steps()
	res.Source = "llm"
	return res, nil
}

// betaTool adapts one toolbox tool to the SDK tool runner. The raw JSON
// input goes to the toolbox, which validates it strictly; a tool error
// becomes an is_error tool_result the model can recover from.
func betaTool(tb *investigate.Toolbox, spec investigate.Spec) anthropic.BetaTool {
	schema := anthropic.BetaToolInputSchemaParam{
		Properties:  spec.Schema["properties"],
		ExtraFields: map[string]any{"additionalProperties": false},
	}
	if req, ok := spec.Schema["required"].([]string); ok {
		schema.Required = req
	}
	name := spec.Name
	return toolrunner.NewBetaTool(name, spec.Description, schema,
		func(ctx context.Context, in json.RawMessage) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			out, err := tb.Run(ctx, name, in)
			if err != nil {
				return anthropic.BetaToolResultBlockParamContentUnion{}, err
			}
			return anthropic.BetaToolResultBlockParamContentUnion{OfText: &anthropic.BetaTextBlockParam{Text: out}}, nil
		})
}

func userPrompt(req investigate.Request, now time.Time, origin string) string {
	var b strings.Builder
	b.WriteString("<question>\n" + req.Question + "\n</question>\n\n")
	fmt.Fprintf(&b, "Window: %s (tools default to it)\n", req.Window)
	if req.ClusterID != "" {
		fmt.Fprintf(&b, "Cluster: %s\n", req.ClusterID)
	} else {
		b.WriteString("Cluster: all clusters (fleet-wide)\n")
	}
	if req.Context != "" {
		fmt.Fprintf(&b, "Asked from dashboard context: %s\n", req.Context)
	}
	fmt.Fprintf(&b, "Current time (UTC): %s\n", now.UTC().Format(time.RFC3339))
	if origin == "demo" {
		b.WriteString("Data source: built-in DEMO data (no live control plane) — say so in the answer.\n")
	}
	return b.String()
}

// ─── output parsing ──────────────────────────────────────────────────────

type llmEvidence struct {
	Kind     string `json:"kind"`
	Title    string `json:"title"`
	Detail   string `json:"detail"`
	LinkPath string `json:"link_path"`
	Query    string `json:"query"`
}

type llmInvestigation struct {
	AnswerMarkdown string        `json:"answer_markdown"`
	SpokenSummary  string        `json:"spoken_summary"`
	Evidence       []llmEvidence `json:"evidence"`
	Actions        []llmAction   `json:"actions"`
}

var evidenceKinds = map[string]bool{
	"cost": true, "anomaly": true, "logs": true, "profile": true,
	"network": true, "event": true, "rightsizing": true, "alert": true,
}

// Output bounds: whatever the model says, the response stays small.
const (
	maxAnswerRunes   = 20000
	maxSpokenRunes   = 1500
	maxEvidenceItems = 20
	maxActions       = 10
)

// parseInvestigation leniently parses the model's final answer and
// applies the guardrails: bounded sizes, whitelisted evidence kinds,
// dashboard-relative links only, and brain.ValidateActions.
func parseInvestigation(raw string) (*kuberov1.InvestigateResponse, error) {
	jsonText, err := extractJSON(raw)
	if err != nil {
		return nil, err
	}
	var li llmInvestigation
	if err := json.Unmarshal([]byte(jsonText), &li); err != nil {
		return nil, fmt.Errorf("unmarshal investigation: %w", err)
	}
	answer := strings.TrimSpace(li.AnswerMarkdown)
	if answer == "" {
		return nil, errors.New("investigation missing answer_markdown")
	}
	spoken := strings.TrimSpace(li.SpokenSummary)
	if spoken == "" {
		spoken = firstSentence(answer)
	}
	res := &kuberov1.InvestigateResponse{
		AnswerMarkdown: truncateRunes(answer, maxAnswerRunes),
		SpokenSummary:  truncateRunes(spoken, maxSpokenRunes),
	}
	for _, e := range li.Evidence {
		if len(res.Evidence) >= maxEvidenceItems {
			break
		}
		if strings.TrimSpace(e.Title) == "" {
			continue
		}
		kind := e.Kind
		if !evidenceKinds[kind] {
			kind = "event"
		}
		res.Evidence = append(res.Evidence, &kuberov1.EvidenceItem{
			Kind:     kind,
			Title:    truncateRunes(strings.TrimSpace(e.Title), 200),
			Detail:   truncateRunes(strings.TrimSpace(e.Detail), 1000),
			LinkPath: safeLinkPath(e.LinkPath),
			Query:    truncateRunes(strings.TrimSpace(e.Query), 1000),
		})
	}
	actions := make([]*kuberov1.ProposedAction, 0, len(li.Actions))
	for i, a := range li.Actions {
		if i >= maxActions {
			break
		}
		id := strings.TrimSpace(a.ID)
		if id == "" {
			id = fmt.Sprintf("act-inv-llm-%d", i+1)
		}
		actions = append(actions, &kuberov1.ProposedAction{
			Id: truncateRunes(id, 80), Title: truncateRunes(a.Title, 200), ImpactMonthlyUsd: a.ImpactMonthlyUSD,
			Risk: a.Risk, Kind: a.Kind, Target: truncateRunes(a.Target, 200), Rationale: truncateRunes(a.Rationale, 1000),
			CrdYaml: a.CrdYAML, Status: brain.StatusProposed,
		})
	}
	// The guardrail: whitelist kinds, clamp impacts, downgrade anything
	// whose crd_yaml isn't a parseable guarded policy CRD.
	res.Actions = brain.ValidateActions(actions)
	return res, nil
}

// safeLinkPath keeps dashboard-relative paths only: the model must not
// be able to put an external (or protocol-relative) link in front of a
// human as "evidence".
func safeLinkPath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, " \t\r\n\\") || len(p) > 1000 {
		return ""
	}
	return p
}

func firstSentence(s string) string {
	s = strings.TrimSpace(strings.TrimLeft(s, "#*- "))
	if i := strings.IndexAny(s, ".\n"); i > 0 {
		return s[:i+1]
	}
	return truncateRunes(s, 300)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
