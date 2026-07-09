// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package llm is the Anthropic-backed advisor brain, active when
// ANTHROPIC_API_KEY is set. It narrates the snapshot and proposes
// guarded actions; every response is parsed leniently and pushed
// through the same validation guardrail as the rules brain. On ANY
// failure — API error, timeout, unparseable output — it falls back to
// the deterministic rules brain, so briefings never 500 because
// Anthropic is down.
package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// requestTimeout bounds a single briefing generation.
const requestTimeout = 60 * time.Second

// systemPrompt is the advisor persona. The CRD schema summaries are
// distilled from services/operator/api/v1 types and config/samples so
// the model can only propose manifests the operator actually accepts.
const systemPrompt = `You are the KubeHero Advisor: a senior SRE / FinOps expert embedded in a Kubernetes cost and operations platform.

Rules you must follow:
- Be concise and concrete. Never invent data: only reference clusters, workloads, dollar amounts, and anomalies present in the snapshot JSON the user provides.
- You are READ-ONLY. You only PROPOSE actions; humans apply them through KubeHero's arming flow. Never suggest imperative kubectl mutations.
- Every proposed action must map to exactly one KubeHero policy CRD — BudgetPolicy, CeilingPolicy, or RightsizingPolicy — or be an investigate-only action with no manifest.
- Prefer conservative defaults: recommend-mode rightsizing, alert-first escalations, humanArm: true.

KubeHero CRD schemas (apiVersion: kubehero.kubehero.io/v1, metadata.namespace: kubehero-system):

1) BudgetPolicy — declarative spending intent.
   spec:
     scope: { clusterSelector|namespaceSelector: <label selector> }   # required
     ceiling: "$100000/mo"                                            # required, "$<amount>/mo" or "/hr"
     hardStop: <bool>                                                 # optional; false = advisory
     humanArm: <bool>                                                 # keep true
     escalation: [ {action: alert|hpa.cap|pod.evict|nodepool.cordon, ratioPercent: 1..100, waitAfter: "2m", channels: ["slack://ops"]} ]
     alertChannels: ["slack://ops"]

2) CeilingPolicy — burn-rate triggered enforcement.
   spec:
     budgetRef: <name of a BudgetPolicy>       # required
     trigger: { burnRateMilli: >=1000, window: "5m" }   # required
     escalation: [ <same steps as BudgetPolicy> ]
     cooldown: "10m"
     humanArm: true

3) RightsizingPolicy — request right-sizing.
   spec:
     scope: { namespaceSelector: <label selector> }     # required
     mode: recommend|apply|shadow                       # use recommend unless told otherwise
     targetUtilization: 1..100
     exclude: [<workload names>]
     safety: { minReplicas: <int>, p95HeadroomPct: <int>, observationWindow: "14d", maxChangePerDay: <int> }

Output format — respond with a SINGLE JSON object and nothing else (no prose, no markdown fences):
{
  "headline": "<one line, <=120 chars>",
  "markdown": "<full briefing in markdown: spend summary, top savings, anomalies, proposed actions>",
  "spoken_script": "<TTS-ready plain prose, 45-90 seconds spoken (~120-220 words), no markdown syntax>",
  "actions": [
    {
      "id": "<stable slug>",
      "title": "<imperative title>",
      "impact_monthly_usd": <number >= 0>,
      "risk": "low|medium|high",
      "kind": "rightsize.requests|ceiling.arm|nodepool.consolidate|workload.investigate",
      "target": "<cluster/namespace/workload>",
      "rationale": "<why, grounded in the snapshot>",
      "crd_yaml": "<complete YAML manifest for one of the three CRDs, or \"\" for workload.investigate>"
    }
  ]
}`

// Brain calls Anthropic and falls back to `Fallback` on any failure.
type Brain struct {
	client   anthropic.Client
	fallback brain.Brain
	log      *slog.Logger
}

// New builds the LLM brain. The Anthropic client reads
// ANTHROPIC_API_KEY from the environment.
func New(fallback brain.Brain, log *slog.Logger) *Brain {
	return &Brain{client: anthropic.NewClient(), fallback: fallback, log: log}
}

func (b *Brain) Generate(ctx context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error) {
	briefing, err := b.generate(ctx, snap)
	if err != nil {
		b.log.Warn("llm brain failed, falling back to rules", "err", err)
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
	user := "Cluster data snapshot (window " + snap.Window + "):\n" + string(payload) +
		"\n\nProduce the briefing JSON now."

	stream := b.client.Messages.NewStreaming(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeOpus4_8,
		MaxTokens: 16000,
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &anthropic.ThinkingConfigAdaptiveParam{}},
		System:    []anthropic.TextBlockParam{{Text: systemPrompt}},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	message := anthropic.Message{}
	for stream.Next() {
		if err := message.Accumulate(stream.Current()); err != nil {
			return nil, fmt.Errorf("accumulate stream: %w", err)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, fmt.Errorf("anthropic stream: %w", err)
	}

	var text strings.Builder
	for _, block := range message.Content {
		if t, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	briefing, err := parseBriefing(text.String())
	if err != nil {
		return nil, fmt.Errorf("parse llm output: %w", err)
	}
	return briefing, nil
}
