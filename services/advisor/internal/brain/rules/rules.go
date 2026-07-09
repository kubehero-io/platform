// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Package rules is the deterministic advisor brain: same inputs, same
// briefing. It is the offline default, the fallback when the LLM tier
// is unavailable or returns garbage, and what the tests exercise.
package rules

import (
	"context"
	"fmt"
	"sort"
	"strings"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
	"github.com/kubehero-io/platform/services/advisor/internal/brain"
	"github.com/kubehero-io/platform/services/advisor/internal/source"
)

// burnRateArmThresholdMilli is the sustained burn rate (× 1000) above
// which the rules brain proposes arming a CeilingPolicy.
const burnRateArmThresholdMilli = 1500

// Brain implements brain.Brain deterministically.
type Brain struct{}

func New() *Brain { return &Brain{} }

func (b *Brain) Generate(_ context.Context, snap *source.Snapshot) (*kuberov1.Briefing, error) {
	actions := buildActions(snap)
	briefing := &kuberov1.Briefing{
		Headline:     headline(snap, actions),
		Markdown:     markdown(snap, actions),
		SpokenScript: spokenScript(snap, actions),
		Source:       "rules",
		Actions:      brain.ValidateActions(actions),
	}
	return briefing, nil
}

// ─── actions ─────────────────────────────────────────────────────────────

func buildActions(snap *source.Snapshot) []*kuberov1.ProposedAction {
	var actions []*kuberov1.ProposedAction

	// Top waste recommendations by monthly impact.
	waste := append([]source.WasteRecommendation(nil), snap.Waste...)
	sort.SliceStable(waste, func(i, j int) bool {
		return waste[i].RecoverableUSDMonth > waste[j].RecoverableUSDMonth
	})
	if len(waste) > 3 {
		waste = waste[:3]
	}
	for _, w := range waste {
		if w.RecoverableUSDMonth <= 0 {
			continue
		}
		target := fmt.Sprintf("%s/%s/%s", w.Cluster, w.Namespace, w.Workload)
		if w.Action == "apply" {
			actions = append(actions, &kuberov1.ProposedAction{
				Id:               "act-rs-" + slug(w.Namespace+"-"+w.Workload),
				Title:            fmt.Sprintf("Rightsize %s in %s", w.Workload, w.Namespace),
				ImpactMonthlyUsd: w.RecoverableUSDMonth,
				Risk:             "low",
				Kind:             brain.KindRightsize,
				Target:           target,
				Rationale: fmt.Sprintf(
					"Requests far exceed observed usage (%s). A recommend-mode RightsizingPolicy surfaces the right-size without touching live pods; ~%s/mo recoverable.",
					w.Signal, money(w.RecoverableUSDMonth)),
				CrdYaml: rightsizingYAML(w),
				Status:  brain.StatusProposed,
			})
		} else {
			actions = append(actions, &kuberov1.ProposedAction{
				Id:               "act-inv-" + slug(w.Namespace+"-"+w.Workload),
				Title:            fmt.Sprintf("Investigate %s in %s", w.Workload, w.Namespace),
				ImpactMonthlyUsd: w.RecoverableUSDMonth,
				Risk:             "low",
				Kind:             brain.KindInvestigate,
				Target:           target,
				Rationale: fmt.Sprintf(
					"Flagged for review (%s), ~%s/mo at stake. Needs a human look before any policy is proposed.",
					w.Signal, money(w.RecoverableUSDMonth)),
				Status: brain.StatusProposed,
			})
		}
	}

	// Burn rate vs budget: propose arming a ceiling when sustained hot.
	if br := snap.BurnRate; br != nil && br.Available && br.BurnRateMilli >= burnRateArmThresholdMilli {
		actions = append(actions, &kuberov1.ProposedAction{
			Id:               "act-ceiling-burn-rate",
			Title:            fmt.Sprintf("Arm burn-rate ceiling (%.1fx budgeted rate)", float64(br.BurnRateMilli)/1000),
			ImpactMonthlyUsd: overspend(snap),
			Risk:             "medium",
			Kind:             brain.KindCeilingArm,
			Target:           fleetTarget(snap),
			Rationale: fmt.Sprintf(
				"Spend is running at %.1fx the budgeted rate over the last 24h. Arming a CeilingPolicy adds an alert-first escalation with humanArm so nothing fires without sign-off.",
				float64(br.BurnRateMilli)/1000),
			CrdYaml: ceilingYAML(br.BurnRateMilli),
			Status:  brain.StatusProposed,
		})
	}

	// Anomaly callouts: investigate-only, warn/critical severity.
	for _, a := range snap.Anomalies {
		if a.Severity != "warn" && a.Severity != "critical" {
			continue
		}
		actions = append(actions, &kuberov1.ProposedAction{
			Id:               "act-inv-" + slug(a.ID),
			Title:            "Investigate: " + a.Title,
			ImpactMonthlyUsd: a.ImpactUSDMonth,
			Risk:             "low",
			Kind:             brain.KindInvestigate,
			Target:           fleetTarget(snap) + "/" + a.Subject,
			Rationale:        a.Detail,
			Status:           brain.StatusProposed,
		})
	}

	return actions
}

// ─── narrative ───────────────────────────────────────────────────────────

func headline(snap *source.Snapshot, actions []*kuberov1.ProposedAction) string {
	recoverable := 0.0
	if snap.Spend != nil {
		recoverable = snap.Spend.FleetRecoverableUSDMonth
	}
	if recoverable == 0 {
		for _, a := range actions {
			recoverable += a.ImpactMonthlyUsd
		}
	}
	return fmt.Sprintf("%s/mo recoverable · %d proposed actions · %d anomalies",
		money(recoverable), len(actions), len(snap.Anomalies))
}

func markdown(snap *source.Snapshot, actions []*kuberov1.ProposedAction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cost briefing (%s window)\n\n", snap.Window)

	if snap.Spend != nil {
		fmt.Fprintf(&b, "Fleet spend is **%s/mo** with **%s/mo** flagged as recoverable.\n\n",
			money(snap.Spend.FleetTotalUSDMonth), money(snap.Spend.FleetRecoverableUSDMonth))
	}
	if br := snap.BurnRate; br != nil && br.Available {
		fmt.Fprintf(&b, "Burn rate is **%.1fx** the budgeted run-rate.\n\n", float64(br.BurnRateMilli)/1000)
	}

	if len(snap.Waste) > 0 {
		b.WriteString("### Top savings\n\n")
		for _, w := range snap.Waste {
			fmt.Fprintf(&b, "- **%s/%s** (%s): %s — ~%s/mo recoverable\n",
				w.Namespace, w.Workload, w.Cluster, w.Signal, money(w.RecoverableUSDMonth))
		}
		b.WriteString("\n")
	}

	if len(snap.Anomalies) > 0 {
		b.WriteString("### Anomalies\n\n")
		for _, a := range snap.Anomalies {
			fmt.Fprintf(&b, "- **%s** — %s (~%s/mo impact)\n", a.Title, a.Detail, money(a.ImpactUSDMonth))
		}
		b.WriteString("\n")
	}

	if len(actions) > 0 {
		b.WriteString("### Proposed actions\n\n")
		b.WriteString("All actions are proposals. Manifests apply through the operator's human-arm flow; nothing executes automatically.\n\n")
		for _, a := range actions {
			fmt.Fprintf(&b, "1. **%s** (%s, risk: %s, ~%s/mo) — %s\n",
				a.Title, a.Kind, a.Risk, money(a.ImpactMonthlyUsd), a.Rationale)
		}
	}
	return strings.TrimSpace(b.String())
}

func spokenScript(snap *source.Snapshot, actions []*kuberov1.ProposedAction) string {
	var parts []string
	parts = append(parts, "Here is your KubeHero briefing.")
	if snap.Spend != nil {
		parts = append(parts, fmt.Sprintf(
			"The fleet is spending about %s a month, and roughly %s of that looks recoverable.",
			moneySpoken(snap.Spend.FleetTotalUSDMonth), moneySpoken(snap.Spend.FleetRecoverableUSDMonth)))
	}
	if br := snap.BurnRate; br != nil && br.Available {
		if br.BurnRateMilli >= burnRateArmThresholdMilli {
			parts = append(parts, fmt.Sprintf(
				"Heads up: spend is burning at %.1f times the budgeted rate, so I am proposing you arm a ceiling policy with alert-first escalation.",
				float64(br.BurnRateMilli)/1000))
		} else {
			parts = append(parts, fmt.Sprintf(
				"Burn rate is %.1f times the budgeted rate, which is within tolerance.",
				float64(br.BurnRateMilli)/1000))
		}
	}
	if len(snap.Waste) > 0 {
		w := snap.Waste[0]
		parts = append(parts, fmt.Sprintf(
			"The single biggest saving is %s in the %s namespace, worth about %s a month.",
			w.Workload, w.Namespace, moneySpoken(w.RecoverableUSDMonth)))
	}
	for _, a := range snap.Anomalies {
		if a.Severity == "warn" || a.Severity == "critical" {
			parts = append(parts, fmt.Sprintf("One anomaly needs eyes: %s. %s.", a.Title, strings.TrimSuffix(a.Detail, ".")))
			break
		}
	}
	parts = append(parts, fmt.Sprintf(
		"In total I have %d proposed actions queued. Nothing runs without your sign-off — every action goes through the human arming flow.",
		len(actions)))
	return strings.Join(parts, " ")
}

// ─── CRD manifests ───────────────────────────────────────────────────────

func rightsizingYAML(w source.WasteRecommendation) string {
	return fmt.Sprintf(`apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata:
  name: advisor-rightsize-%s
  namespace: kubehero-system
spec:
  scope:
    namespaceSelector:
      matchLabels: { kubernetes.io/metadata.name: %q }
  mode: recommend
  targetUtilization: 70
  safety:
    minReplicas: 2
    p95HeadroomPct: 40
    observationWindow: "14d"
`, slug(w.Workload), w.Namespace)
}

func ceilingYAML(burnRateMilli int32) string {
	trigger := burnRateMilli
	if trigger < 1000 {
		trigger = 1000
	}
	return fmt.Sprintf(`apiVersion: kubehero.kubehero.io/v1
kind: CeilingPolicy
metadata:
  name: advisor-burn-rate-guard
  namespace: kubehero-system
spec:
  budgetRef: prod-monthly-ceiling
  trigger:
    burnRateMilli: %d
    window: "1h"
  escalation:
    - action: alert
      channels: ["slack://ops"]
  cooldown: "30m"
  humanArm: true
`, trigger)
}

// ─── helpers ─────────────────────────────────────────────────────────────

// overspend estimates the monthly dollars above budget the current burn
// rate represents.
func overspend(snap *source.Snapshot) float64 {
	if snap.Spend == nil || snap.BurnRate == nil {
		return 0
	}
	over := float64(snap.BurnRate.BurnRateMilli-1000) / 1000 * snap.Spend.FleetTotalUSDMonth
	if over < 0 {
		return 0
	}
	return over
}

func fleetTarget(snap *source.Snapshot) string {
	if snap.ClusterID != "" {
		return snap.ClusterID
	}
	return "fleet"
}

// slug produces a lowercase RFC-1123-ish name fragment.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		out = "unnamed"
	}
	return out
}

// money renders whole dollars with thousands separators, e.g. "$18,400".
func money(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	s := fmt.Sprintf("%.0f", v)
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

// moneySpoken renders a TTS-friendly amount ("18,400 dollars").
func moneySpoken(v float64) string {
	return strings.TrimPrefix(money(v), "$") + " dollars"
}
