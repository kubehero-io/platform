// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/brain/rules"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

func briefingJSON() string {
	return mustJSON(map[string]any{
		"headline":      "Payments errors are costing you money",
		"markdown":      "## Briefing\n\nPayments error rate is 6.3%.",
		"spoken_script": "Here is your briefing.",
		"actions": []map[string]any{
			{"id": "a", "title": "Rightsize checkout-api", "impact_monthly_usd": 6200, "risk": "low",
				"kind": brain.KindRightsize, "target": "eks-use1-prod/payments/checkout-api", "rationale": "x", "crd_yaml": validPolicy},
			{"id": "b", "title": "Evict everything", "impact_monthly_usd": 1, "risk": "high",
				"kind": "pod.evict", "target": "all", "rationale": "x", "crd_yaml": ""},
		},
	})
}

func TestBriefingBrainUsesClaude(t *testing.T) {
	f := newFakeAnthropic(t, fakeTurn{text: briefingJSON()})
	var logs bytes.Buffer
	cfg := f.config()
	cfg.Model = "claude-sonnet-5"
	b := New(rules.New(), slog.New(slog.NewJSONHandler(&logs, nil)), cfg)
	got, err := b.Generate(context.Background(), source.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "llm" {
		t.Fatalf("source = %q (logs %s)", got.Source, logs.String())
	}
	if len(got.Actions) != 2 || got.Actions[1].Kind != brain.KindInvestigate {
		t.Errorf("guardrail not applied: %+v", got.Actions)
	}
	req := f.request(0)
	if req["model"] != "claude-sonnet-5" {
		t.Errorf("KUBEHERO_ADVISOR_MODEL override not honoured: %v", req["model"])
	}
	if req["fallbacks"] != "default" || !hasBeta(f.betas[0], "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks not requested: %v / %q", req["fallbacks"], f.betas[0])
	}
	msgs := mustJSON(req["messages"])
	for _, want := range []string{"alertsFiring", "costAllocationTopNamespaces", "logErrorVolume", "topRightsizing"} {
		if !strings.Contains(msgs, want) {
			t.Errorf("snapshot enrichment %s not sent to the model", want)
		}
	}
}

func TestBriefingBrainRefusalFallsBack(t *testing.T) {
	f := newFakeAnthropic(t, fakeTurn{refusal: "general_harms"})
	var logs bytes.Buffer
	b := New(rules.New(), slog.New(slog.NewJSONHandler(&logs, nil)), f.config())
	got, err := b.Generate(context.Background(), source.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "rules" {
		t.Errorf("source = %q, want rules", got.Source)
	}
	if !strings.Contains(logs.String(), "general_harms") {
		t.Errorf("refusal category not logged: %s", logs.String())
	}
}
