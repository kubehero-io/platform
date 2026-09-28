// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

package alerts

import (
	"time"

	kuberov1 "github.com/kubehero-io/platform/packages/proto/gen/go/kubehero/v1"
)

// demoAlerts is served without ClickHouse (and with demo fixtures
// allowed): nothing can be evaluated, so the page shows what alerting
// looks like — every alert carries the label source=demo.
func demoAlerts(now time.Time) []*kuberov1.Alert {
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	lbl := func(kv ...string) map[string]string {
		m := map[string]string{"source": "demo"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	return []*kuberov1.Alert{
		{Id: "alrt-demo-01", RuleId: "demo-rule-budget", RuleName: "Budget burn over 1.5x", Kind: KindBudget, State: StateFiring,
			Severity: "critical", Labels: lbl("alertname", "Budget burn over 1.5x", "policy", "ml-gpu-ceiling", "cluster", "aks-westeu-prod-01", "severity", "critical"),
			Value: 1.84, Summary: "budget ml-gpu-ceiling burning at 1.84× its ceiling",
			Description: "A BudgetPolicy's last-hour spend rate, extrapolated to a month, exceeds 1.5× its ceiling for 15 minutes.",
			StartedAt:   at(52 * time.Minute), FiredAt: at(37 * time.Minute), LastEvalAt: at(20 * time.Second), LinkPath: "/budgets"},
		{Id: "alrt-demo-02", RuleId: "demo-rule-oom", RuleName: "OOM kills", Kind: KindEvent, State: StateFiring,
			Severity: "warn", Labels: lbl("alertname", "OOM kills", "cluster", "eks-use1-prod", "namespace", "payments", "workload", "ledger", "severity", "warn"),
			Value: 4, Summary: "payments/ledger OOM-killed 4× in 15m",
			Description: "A workload was OOM-killed 3 or more times in 15 minutes.",
			StartedAt:   at(9 * time.Minute), FiredAt: at(9 * time.Minute), LastEvalAt: at(20 * time.Second),
			LinkPath: "/workloads/eks-use1-prod/payments/ledger"},
		{Id: "alrt-demo-03", RuleId: "demo-rule-egress", RuleName: "Internet egress over $5/h", Kind: KindNetwork, State: StatePending,
			Severity: "warn", Labels: lbl("alertname", "Internet egress over $5/h", "cluster", "eks-use1-prod", "namespace", "ml-inference", "workload", "embedder", "severity", "warn"),
			Value: 6.2, Summary: "ml-inference/embedder egress at $6.2/h",
			Description: "A workload's internet egress costs more than $5/hour (15-minute rate), sustained for 15 minutes.",
			StartedAt:   at(6 * time.Minute), LastEvalAt: at(20 * time.Second), LinkPath: "/network?namespace=ml-inference"},
		{Id: "alrt-demo-04", RuleId: "demo-rule-logs", RuleName: "Error log spike", Kind: KindLogs, State: StateResolved,
			Severity: "warn", Labels: lbl("alertname", "Error log spike", "cluster", "gke-usc1-prod", "namespace", "checkout", "workload", "cart", "severity", "warn"),
			Value: 42, Summary: "checkout/cart: 42 error lines in 5m",
			Description: "A workload logged more than 100 error/fatal lines in 5 minutes, sustained for 5 minutes.",
			StartedAt:   at(3 * time.Hour), FiredAt: at(170 * time.Minute), ResolvedAt: at(2 * time.Hour), LastEvalAt: at(20 * time.Second),
			LinkPath: `/logs?query=%7Bnamespace%3D%22checkout%22%2Cworkload%3D%22cart%22%7D`},
	}
}

// demoSamples previews a rule without data: two plausible series, one
// over and one under the threshold, labelled source=demo.
func demoSamples(r *Rule) []Sample {
	hi, lo := r.Threshold*1.4+1, r.Threshold*0.5
	if r.Op == "<" || r.Op == "<=" {
		hi, lo = lo, hi
	}
	return []Sample{
		{Labels: map[string]string{"namespace": "checkout", "workload": "cart", "source": "demo"}, Value: hi},
		{Labels: map[string]string{"namespace": "edge", "workload": "frontend-gateway", "source": "demo"}, Value: lo},
	}
}
