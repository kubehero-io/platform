// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package llm

import (
	"strings"
	"testing"

	"github.com/kubehero-io/platform/services/advisor/internal/brain"
)

const validRightsizingYAML = `apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata:
  name: advisor-rightsize-checkout-api
  namespace: kubehero-system
spec:
  scope:
    namespaceSelector:
      matchLabels: { env: prod }
  mode: recommend
`

func TestParseBriefingCleanJSON(t *testing.T) {
	raw := `{
		"headline": "Save $6,200/mo on checkout-api",
		"markdown": "## Briefing\n\nDetails here.",
		"spoken_script": "Here is your briefing.",
		"actions": [{
			"id": "act-1",
			"title": "Rightsize checkout-api",
			"impact_monthly_usd": 6200,
			"risk": "low",
			"kind": "rightsize.requests",
			"target": "eks-use1-prod/payments/checkout-api",
			"rationale": "cpu.req=16 used=0.41",
			"crd_yaml": ` + jsonString(validRightsizingYAML) + `
		}]
	}`
	b, err := parseBriefing(raw)
	if err != nil {
		t.Fatalf("parseBriefing: %v", err)
	}
	if b.Source != "llm" {
		t.Errorf("source = %q, want llm", b.Source)
	}
	if len(b.Actions) != 1 {
		t.Fatalf("actions = %d, want 1", len(b.Actions))
	}
	a := b.Actions[0]
	if a.Kind != brain.KindRightsize {
		t.Errorf("valid action was downgraded: kind = %q", a.Kind)
	}
	if a.Status != brain.StatusProposed {
		t.Errorf("status = %q, want proposed", a.Status)
	}
}

func TestParseBriefingWithFencesAndProse(t *testing.T) {
	raw := "Sure! Here is the briefing you asked for:\n\n```json\n" +
		`{"headline":"h","markdown":"m","spoken_script":"s","actions":[]}` +
		"\n```\nLet me know if you need anything else!"
	b, err := parseBriefing(raw)
	if err != nil {
		t.Fatalf("parseBriefing with fences: %v", err)
	}
	if b.Headline != "h" || b.Markdown != "m" || b.SpokenScript != "s" {
		t.Errorf("unexpected briefing: %+v", b)
	}
}

func TestParseBriefingDowngrades(t *testing.T) {
	tests := []struct {
		name   string
		action string
	}{
		{
			name: "unknown kind",
			action: `{"id":"a","title":"t","impact_monthly_usd":10,"risk":"low",
				"kind":"cluster.delete","target":"x","rationale":"r","crd_yaml":""}`,
		},
		{
			name: "yaml does not parse",
			action: `{"id":"a","title":"t","impact_monthly_usd":10,"risk":"low",
				"kind":"rightsize.requests","target":"x","rationale":"r",
				"crd_yaml":"{{ not yaml: ["}`,
		},
		{
			name: "yaml kind not whitelisted",
			action: `{"id":"a","title":"t","impact_monthly_usd":10,"risk":"low",
				"kind":"rightsize.requests","target":"x","rationale":"r",
				"crd_yaml":"apiVersion: v1\nkind: Deployment\nmetadata:\n  name: hack\n"}`,
		},
		{
			name: "empty manifest on applyable kind",
			action: `{"id":"a","title":"t","impact_monthly_usd":10,"risk":"low",
				"kind":"ceiling.arm","target":"x","rationale":"r","crd_yaml":""}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"headline":"h","markdown":"m","spoken_script":"s","actions":[` + tc.action + `]}`
			b, err := parseBriefing(raw)
			if err != nil {
				t.Fatalf("parseBriefing: %v", err)
			}
			a := b.Actions[0]
			if a.Kind != brain.KindInvestigate {
				t.Errorf("kind = %q, want downgraded to workload.investigate", a.Kind)
			}
			if a.CrdYaml != "" {
				t.Errorf("downgraded action still carries crd_yaml: %q", a.CrdYaml)
			}
		})
	}
}

func TestParseBriefingClampsAndDefaults(t *testing.T) {
	raw := `{"headline":"h","markdown":"m","spoken_script":"",
		"actions":[
			{"id":"","title":"t","impact_monthly_usd":-500,"risk":"catastrophic",
			 "kind":"workload.investigate","target":"x","rationale":"r","crd_yaml":""}
		]}`
	b, err := parseBriefing(raw)
	if err != nil {
		t.Fatalf("parseBriefing: %v", err)
	}
	if b.SpokenScript != "h" {
		t.Errorf("empty spoken_script should fall back to headline, got %q", b.SpokenScript)
	}
	a := b.Actions[0]
	if a.Id == "" {
		t.Error("missing id should be assigned")
	}
	if a.ImpactMonthlyUsd != 0 {
		t.Errorf("negative impact should clamp to 0, got %f", a.ImpactMonthlyUsd)
	}
	if a.Risk != "medium" {
		t.Errorf("invalid risk should default to medium, got %q", a.Risk)
	}
}

func TestParseBriefingGarbage(t *testing.T) {
	for _, raw := range []string{
		"",
		"I'm sorry, I can't produce a briefing right now.",
		"```json\nnot json at all\n```",
		`{"headline":"","markdown":"","actions":[]}`,        // empty narrative
		`{"headline": "h", "markdown": "m", "actions": [{}`, // truncated
	} {
		if _, err := parseBriefing(raw); err == nil {
			t.Errorf("parseBriefing(%q) succeeded, want error (→ rules fallback)", truncate(raw))
		}
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// jsonString encodes s as a JSON string literal for test fixtures.
func jsonString(s string) string {
	return `"` + strings.NewReplacer("\n", `\n`, `"`, `\"`).Replace(s) + `"`
}
