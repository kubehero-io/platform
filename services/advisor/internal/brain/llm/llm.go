// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package llm is the Claude-backed advisor tier, active when
// ANTHROPIC_API_KEY is set: the briefing brain here and the agentic
// investigator in investigate.go. Every request streams, uses adaptive
// thinking, opts into server-side refusal fallbacks, and asks for a
// schema-constrained JSON answer that is still parsed leniently and
// pushed through the same guardrail as the rules tier. On ANY failure —
// API error, timeout, refusal, unparseable output — it falls back to the
// deterministic rules tier, so nothing 500s because Anthropic is down.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// requestTimeout bounds a single briefing generation.
const requestTimeout = 90 * time.Second

// actionSchema is the JSON schema of one proposed action (shared by
// briefings and investigations).
var actionSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"id":                 map[string]any{"type": "string", "description": "stable slug"},
		"title":              map[string]any{"type": "string", "description": "imperative title"},
		"impact_monthly_usd": map[string]any{"type": "number", "description": "monthly USD impact, >= 0"},
		"risk":               map[string]any{"type": "string", "enum": []string{"low", "medium", "high"}},
		"kind": map[string]any{"type": "string", "enum": []string{
			brain.KindRightsize, brain.KindCeilingArm, brain.KindConsolidate, brain.KindInvestigate}},
		"target":    map[string]any{"type": "string", "description": "cluster/namespace/workload"},
		"rationale": map[string]any{"type": "string", "description": "why, grounded in the data"},
		"crd_yaml":  map[string]any{"type": "string", "description": "complete CRD manifest, or empty for workload.investigate"},
	},
	"required":             []string{"id", "title", "impact_monthly_usd", "risk", "kind", "target", "rationale", "crd_yaml"},
	"additionalProperties": false,
}

var briefingSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"headline":      map[string]any{"type": "string"},
		"markdown":      map[string]any{"type": "string"},
		"spoken_script": map[string]any{"type": "string"},
		"actions":       map[string]any{"type": "array", "items": actionSchema},
	},
	"required":             []string{"headline", "markdown", "spoken_script", "actions"},
	"additionalProperties": false,
}

// Brain calls Claude and falls back to `fallback` on any failure.
type Brain struct {
	client   anthropic.Client
	cfg      Config
	fallback brain.Brain
	log      *slog.Logger
}

// New builds the LLM briefing brain.
func New(fallback brain.Brain, log *slog.Logger, cfg Config) *Brain {
	return &Brain{client: cfg.client(), cfg: cfg, fallback: fallback, log: log}
}

func (b *Brain) Generate(ctx context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error) {
	briefing, err := b.generate(ctx, snap)
	if err != nil {
		attrs := []any{"err", err}
		if r, ok := IsRefusal(err); ok {
			attrs = append(attrs, "refusal_category", r.Category)
		}
		b.log.Warn("llm brain failed, falling back to rules", attrs...)
		return b.fallback.Generate(ctx, snap)
	}
	return briefing, nil
}

func (b *Brain) generate(ctx context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	payload, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("marshal snapshot: %w", err)
	}
	user := "Cluster data snapshot (window " + snap.Window + "):\n<snapshot>\n" + string(payload) +
		"\n</snapshot>\n\nWrite the briefing."

	params := b.cfg.baseParams(briefingSystemPrompt)
	params.Messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(user)),
	}
	params.OutputConfig = anthropic.BetaOutputConfigParam{
		Format: anthropic.BetaJSONOutputFormatParam{Schema: briefingSchema},
	}

	stream := b.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	message := anthropic.BetaMessage{}
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("accumulate stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic stream: %w", err)
	}
	if err := checkStop(&message); err != nil {
		return nil, err
	}
	if model, fell := servedBy(&message); fell {
		b.log.Info("briefing served by server-side fallback model", "model", model)
	}
	briefing, err := parseBriefing(messageText(&message))
	if err != nil {
		return nil, fmt.Errorf("parse llm output: %w", err)
	}
	return briefing, nil
}
