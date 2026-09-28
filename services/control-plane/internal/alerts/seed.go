// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import "time"

// DefaultRules is the rule set seeded on the first boot that finds no
// rules (once, ever — deleting them doesn't bring them back). They are
// enabled with no channels: alerts show in the UI and page nobody until
// an admin adds channels. Thresholds are deliberately conservative
// starting points, documented in docs/alerting.md.
func DefaultRules() []*Rule {
	by := "kubehero (default rule set)"
	return []*Rule{
		{
			Name: "Error log spike", Kind: KindLogs, Severity: "warn", Enabled: true, CreatedBy: by,
			Description: "A workload logged more than 100 error/fatal lines in 5 minutes, sustained for 5 minutes.",
			Query:       `sum by (cluster, namespace, workload) (count_over_time({level=~"error|fatal"}[5m]))`,
			Op:          ">", Threshold: 100, PendingFor: 5 * time.Minute, EvalInterval: time.Minute,
			Annotations: map[string]string{"summary": "{{ $labels.namespace }}/{{ $labels.workload }}: {{ $value }} error lines in 5m"},
		},
		{
			Name: "OOM kills", Kind: KindEvent, Severity: "warn", Enabled: true, CreatedBy: by,
			Description: "A workload was OOM-killed 3 or more times in 15 minutes.",
			Query:       `events{kind="oom_killed"}[15m] by (cluster, namespace, workload)`,
			Op:          ">=", Threshold: 3, EvalInterval: time.Minute,
			Annotations: map[string]string{"summary": "{{ $labels.namespace }}/{{ $labels.workload }} OOM-killed {{ $value }}× in 15m"},
		},
		{
			Name: "Spend anomaly over $1k/mo", Kind: KindAnomaly, Severity: "warn", Enabled: true, CreatedBy: by,
			Description: "A workload's last-hour spend deviates from its 24h baseline (|z| ≥ 3) with more than $1,000/month impact.",
			Query:       `anomaly{kind="spend"}`,
			Op:          ">", Threshold: 1000, EvalInterval: 5 * time.Minute,
			Annotations: map[string]string{"summary": "{{ $labels.namespace }}/{{ $labels.workload }} spend anomaly: ${{ $value }}/mo impact"},
		},
		{
			Name: "Budget burn over 1.5x", Kind: KindBudget, Severity: "critical", Enabled: true, CreatedBy: by,
			Description: "A BudgetPolicy's last-hour spend rate, extrapolated to a month, exceeds 1.5× its ceiling for 15 minutes.",
			Query:       `*[1h]`,
			Op:          ">", Threshold: 1.5, PendingFor: 15 * time.Minute, EvalInterval: 5 * time.Minute,
			Annotations: map[string]string{"summary": "budget {{ $labels.policy }} burning at {{ $value }}× its ceiling"},
		},
		{
			Name: "Internet egress over $5/h", Kind: KindNetwork, Severity: "warn", Enabled: true, CreatedBy: by,
			Description: "A workload's internet egress costs more than $5/hour (15-minute rate), sustained for 15 minutes.",
			Query:       `network{egress="true"} by (cluster, namespace, workload)`,
			Op:          ">", Threshold: 5, PendingFor: 15 * time.Minute, EvalInterval: time.Minute,
			Annotations: map[string]string{"summary": "{{ $labels.namespace }}/{{ $labels.workload }} egress at ${{ $value }}/h"},
		},
	}
}
