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

// errorSpikeRatio / errorSpikeMinLines decide when an error-log series
// counts as a spike worth an investigate proposal.
const (
	errorSpikeRatio    = 3.0
	errorSpikeMinLines = 100
)

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
	covered := map[string]bool{} // "namespace/workload" already proposed

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
		covered[w.Namespace+"/"+w.Workload] = true
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
				CrdYaml: rightsizingYAML(w.Namespace, w.Workload),
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

	// Measured rightsizing the waste list didn't already cover.
	for _, r := range snap.Rightsizing {
		key := r.Namespace + "/" + r.Workload
		if covered[key] {
			continue
		}
		switch {
		case r.OOMKills > 0:
			covered[key] = true
			actions = append(actions, &kuberov1.ProposedAction{
				Id:     "act-inv-oom-" + slug(r.Namespace+"-"+r.Workload),
				Title:  fmt.Sprintf("Memory pressure on %s/%s (%d OOM kills)", r.Namespace, r.Workload, r.OOMKills),
				Risk:   "medium",
				Kind:   brain.KindInvestigate,
				Target: fmt.Sprintf("%s/%s/%s", r.Cluster, r.Namespace, r.Workload),
				Rationale: fmt.Sprintf("Container %s was OOM-killed %d times in the window; the recommender suggests sizing memory up. %s",
					r.Container, r.OOMKills, r.Reason),
				Status: brain.StatusProposed,
			})
		case r.SavingsUSDMonth > 0 && (r.Confidence == "high" || r.Confidence == "medium"):
			covered[key] = true
			actions = append(actions, &kuberov1.ProposedAction{
				Id:               "act-rs-" + slug(r.Namespace+"-"+r.Workload),
				Title:            fmt.Sprintf("Rightsize %s in %s", r.Workload, r.Namespace),
				ImpactMonthlyUsd: r.SavingsUSDMonth,
				Risk:             "low",
				Kind:             brain.KindRightsize,
				Target:           fmt.Sprintf("%s/%s/%s", r.Cluster, r.Namespace, r.Workload),
				Rationale: fmt.Sprintf("%s (%s confidence). A recommend-mode RightsizingPolicy surfaces it without touching live pods; ~%s/mo.",
					strings.TrimSuffix(r.Reason, "."), r.Confidence, money(r.SavingsUSDMonth)),
				CrdYaml: rightsizingYAML(r.Namespace, r.Workload),
				Status:  brain.StatusProposed,
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

	// Critical alerts that are firing right now.
	alerted := map[string]bool{}
	for _, al := range snap.FiringAlerts {
		if al.Severity != "critical" {
			continue
		}
		if al.Namespace != "" {
			alerted[al.Namespace] = true
		}
		actions = append(actions, &kuberov1.ProposedAction{
			Id:        "act-inv-alert-" + slug(al.Name),
			Title:     "Investigate firing alert: " + al.Name,
			Risk:      "low",
			Kind:      brain.KindInvestigate,
			Target:    fleetTarget(snap) + "/" + nonEmpty(al.Namespace, "fleet"),
			Rationale: fmt.Sprintf("%s (firing since %s). Ask the advisor to investigate for logs, profiles and cost context.", al.Summary, al.FiredAt),
			Status:    brain.StatusProposed,
		})
	}

	// Error-log spikes no alert already covers.
	if le := snap.LogErrors; le != nil {
		for _, ns := range le.ByNamespace {
			if ns.SpikeRatio < errorSpikeRatio || ns.Lines < errorSpikeMinLines || alerted[ns.Namespace] {
				continue
			}
			actions = append(actions, &kuberov1.ProposedAction{
				Id:     "act-inv-errors-" + slug(ns.Namespace),
				Title:  fmt.Sprintf("Investigate error-log spike in %s", ns.Namespace),
				Risk:   "low",
				Kind:   brain.KindInvestigate,
				Target: fleetTarget(snap) + "/" + ns.Namespace,
				Rationale: fmt.Sprintf("%s error lines in the window, running %.1fx the earlier rate — no alert covers it yet.",
					count(ns.Lines), ns.SpikeRatio),
				Status: brain.StatusProposed,
			})
		}
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
	h := fmt.Sprintf("%s/mo recoverable · %d proposed actions · %d anomalies",
		money(recoverable), len(actions), len(snap.Anomalies))
	if n := len(snap.FiringAlerts); n > 0 {
		h += fmt.Sprintf(" · %d alerts firing", n)
	}
	return h
}

func markdown(snap *source.Snapshot, actions []*kuberov1.ProposedAction) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Cost briefing (%s window)\n\n", snap.Window)

	if snap.Spend != nil {
		fmt.Fprintf(&b, "Fleet spend is **%s/mo** with **%s/mo** flagged as recoverable.\n\n",
			money(snap.Spend.FleetTotalUSDMonth), money(snap.Spend.FleetRecoverableUSDMonth))
	}
	if e := snap.Efficiency; e != nil {
		fmt.Fprintf(&b, "Efficiency score **%.0f/100** (CPU %s, memory %s used of requested; %s/mo idle).\n\n",
			e.Score, pct(e.CPUEfficiency), pct(e.RAMEfficiency), money(e.IdleUSDMonth))
	}
	if br := snap.BurnRate; br != nil && br.Available {
		fmt.Fprintf(&b, "Burn rate is **%.1fx** the budgeted run-rate.\n\n", float64(br.BurnRateMilli)/1000)
	}

	if len(snap.FiringAlerts) > 0 {
		b.WriteString("### Alerts firing\n\n")
		for _, a := range snap.FiringAlerts {
			fmt.Fprintf(&b, "- **%s** (%s) — %s\n", a.Name, a.Severity, a.Summary)
		}
		b.WriteString("\n")
	}

	if len(snap.CostAllocation) > 0 {
		b.WriteString("### Where the money goes\n\n")
		for _, c := range snap.CostAllocation {
			fmt.Fprintf(&b, "- **%s**: %s in the window (~%s/mo) · CPU %s / memory %s efficient\n",
				c.Namespace, money(c.CostUSD), money(c.CostUSDMonth), pct(c.CPUEfficiency), pct(c.RAMEfficiency))
		}
		b.WriteString("\n")
	}

	if len(snap.Waste) > 0 {
		b.WriteString("### Top savings\n\n")
		for _, w := range snap.Waste {
			fmt.Fprintf(&b, "- **%s/%s** (%s): %s — ~%s/mo recoverable\n",
				w.Namespace, w.Workload, w.Cluster, w.Signal, money(w.RecoverableUSDMonth))
		}
		b.WriteString("\n")
	}

	if len(snap.Rightsizing) > 0 {
		b.WriteString("### Measured rightsizing\n\n")
		for _, r := range snap.Rightsizing {
			line := fmt.Sprintf("- **%s/%s** · %s: %s", r.Namespace, r.Workload, r.Container, r.Reason)
			if r.SavingsUSDMonth >= 0 {
				line += fmt.Sprintf(" — ~%s/mo (%s confidence)", money(r.SavingsUSDMonth), r.Confidence)
			} else {
				line += fmt.Sprintf(" — sizing up costs ~%s/mo", money(-r.SavingsUSDMonth))
			}
			b.WriteString(line + "\n")
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

	if le := snap.LogErrors; le != nil && le.TotalLines > 0 {
		b.WriteString("### Error logs\n\n")
		fmt.Fprintf(&b, "%s error lines in the window.\n\n", count(le.TotalLines))
		for _, ns := range le.ByNamespace {
			note := ""
			if ns.SpikeRatio >= errorSpikeRatio {
				note = fmt.Sprintf(" — **spiking %.1fx**", ns.SpikeRatio)
			}
			fmt.Fprintf(&b, "- %s: %s lines%s\n", ns.Namespace, count(ns.Lines), note)
		}
		b.WriteString("\n")
	}

	if len(snap.NetworkTop) > 0 {
		b.WriteString("### Network spend\n\n")
		for _, n := range snap.NetworkTop {
			fmt.Fprintf(&b, "- **%s/%s** → %s: %s/mo (egress %s, cross-zone %s)\n",
				n.Namespace, n.Workload, n.TopDestination, money(n.TotalUSDMonth), money(n.EgressUSDMonth), money(n.CrossZoneUSDMonth))
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
	for _, a := range snap.FiringAlerts {
		if a.Severity == "critical" {
			parts = append(parts, fmt.Sprintf("First, a critical alert is firing: %s.", strings.TrimSuffix(a.Summary, ".")))
			break
		}
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

func rightsizingYAML(namespace, workload string) string {
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
`, slug(workload), namespace)
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

// RightsizingYAML exposes the recommend-mode RightsizingPolicy manifest
// so the rules investigator proposes exactly what briefings propose.
func RightsizingYAML(namespace, workload string) string { return rightsizingYAML(namespace, workload) }

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

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Slug produces a lowercase RFC-1123-ish name fragment.
func Slug(s string) string { return slug(s) }

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
		out = strings.Trim(out[:40], "-")
	}
	if out == "" {
		out = "unnamed"
	}
	return out
}

// Money renders whole dollars with thousands separators, e.g. "$18,400".
func Money(v float64) string { return money(v) }

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

// count renders an integer with thousands separators.
func count(n int64) string { return strings.TrimPrefix(money(float64(n)), "$") }

func pct(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }

// moneySpoken renders a TTS-friendly amount ("18,400 dollars").
func moneySpoken(v float64) string {
	return strings.TrimPrefix(money(v), "$") + " dollars"
}
