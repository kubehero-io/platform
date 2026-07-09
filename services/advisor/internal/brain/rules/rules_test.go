// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package rules

import (
	"context"
	"strings"
	"testing"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

func TestGenerateTableDriven(t *testing.T) {
	demo := source.DemoSnapshot()

	hotBurn := source.DemoSnapshot()
	hotBurn.BurnRate = &source.BurnRate{BurnRateMilli: 2100, Available: true, Source: "demo"}

	coldBurn := source.DemoSnapshot()
	coldBurn.BurnRate = &source.BurnRate{BurnRateMilli: 900, Available: true, Source: "demo"}

	empty := &source.Snapshot{Origin: "control-plane", Window: "24h"}

	tests := []struct {
		name        string
		snap        *source.Snapshot
		wantKinds   map[string]int // kind -> min count
		forbidKinds []string
	}{
		{
			name: "demo fixture proposes rightsize + investigate",
			snap: demo,
			wantKinds: map[string]int{
				brain.KindRightsize:   2, // checkout-api + batch-etl (action=apply)
				brain.KindInvestigate: 2, // model-server (review) + warn anomaly
			},
			forbidKinds: []string{brain.KindCeilingArm}, // 1350 < 1500 threshold
		},
		{
			name:      "hot burn rate arms a ceiling",
			snap:      hotBurn,
			wantKinds: map[string]int{brain.KindCeilingArm: 1},
		},
		{
			name:        "cool burn rate proposes no ceiling",
			snap:        coldBurn,
			forbidKinds: []string{brain.KindCeilingArm},
		},
		{
			name:        "empty snapshot yields empty-but-valid briefing",
			snap:        empty,
			forbidKinds: []string{brain.KindRightsize, brain.KindCeilingArm},
		},
	}

	b := New()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.Generate(context.Background(), tc.snap)
			if err != nil {
				t.Fatalf("Generate: %v", err)
			}
			if got.Source != "rules" {
				t.Errorf("source = %q, want rules", got.Source)
			}
			if got.Headline == "" || got.Markdown == "" || got.SpokenScript == "" {
				t.Errorf("briefing has empty narrative fields: %+v", got)
			}
			counts := map[string]int{}
			for _, a := range got.Actions {
				counts[a.Kind]++
				assertActionInvariants(t, a)
			}
			for kind, min := range tc.wantKinds {
				if counts[kind] < min {
					t.Errorf("kind %s count = %d, want >= %d (all: %v)", kind, counts[kind], min, counts)
				}
			}
			for _, kind := range tc.forbidKinds {
				if counts[kind] > 0 {
					t.Errorf("kind %s unexpectedly present (%d)", kind, counts[kind])
				}
			}
		})
	}
}

func assertActionInvariants(t *testing.T, a *kuberov1.ProposedAction) {
	t.Helper()
	if a.Status != brain.StatusProposed {
		t.Errorf("action %s status = %q, want proposed", a.Id, a.Status)
	}
	if a.ImpactMonthlyUsd < 0 {
		t.Errorf("action %s has negative impact %f", a.Id, a.ImpactMonthlyUsd)
	}
	switch a.Kind {
	case brain.KindInvestigate:
		if a.CrdYaml != "" {
			t.Errorf("investigate action %s carries a manifest", a.Id)
		}
	case brain.KindRightsize:
		if !strings.Contains(a.CrdYaml, "kind: RightsizingPolicy") {
			t.Errorf("rightsize action %s manifest missing RightsizingPolicy:\n%s", a.Id, a.CrdYaml)
		}
		if !strings.Contains(a.CrdYaml, "mode: recommend") {
			t.Errorf("rightsize action %s must be recommend-mode:\n%s", a.Id, a.CrdYaml)
		}
	case brain.KindCeilingArm:
		if !strings.Contains(a.CrdYaml, "kind: CeilingPolicy") {
			t.Errorf("ceiling action %s manifest missing CeilingPolicy:\n%s", a.Id, a.CrdYaml)
		}
		if !strings.Contains(a.CrdYaml, "humanArm: true") {
			t.Errorf("ceiling action %s must set humanArm: true:\n%s", a.Id, a.CrdYaml)
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	b := New()
	one, err := b.Generate(context.Background(), source.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	two, err := b.Generate(context.Background(), source.DemoSnapshot())
	if err != nil {
		t.Fatal(err)
	}
	if one.Markdown != two.Markdown || one.Headline != two.Headline || one.SpokenScript != two.SpokenScript {
		t.Error("rules brain must be deterministic for identical snapshots")
	}
	if len(one.Actions) != len(two.Actions) {
		t.Fatalf("action counts differ: %d vs %d", len(one.Actions), len(two.Actions))
	}
	for i := range one.Actions {
		if one.Actions[i].Id != two.Actions[i].Id {
			t.Errorf("action %d id differs: %s vs %s", i, one.Actions[i].Id, two.Actions[i].Id)
		}
	}
}

func TestMoney(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "$0"},
		{950, "$950"},
		{18400, "$18,400"},
		{128400, "$128,400"},
		{1234567, "$1,234,567"},
	} {
		if got := money(tc.in); got != tc.want {
			t.Errorf("money(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
