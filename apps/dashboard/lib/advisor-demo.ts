// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Demo briefing fixture for the Advisor page. Served whenever ADVISOR_URL
// is unset or the advisor service is unreachable, so the page always works
// standalone. Shape matches the AdvisorService wire contract exactly —
// see lib/api/types.ts (AdvisorBriefingDTO).
//
// The clusters/workloads/dollar figures line up with the rest of the demo
// fleet (lib/api/waste.ts, lib/api/capacity.ts) so cross-navigating pages
// tells one coherent story.

import type { AdvisorBriefingDTO } from "./api/types";

// 2026-07-09 06:00:00 UTC — the morning briefing.
const GENERATED_AT_UNIX = 1_783_576_800;

const MARKDOWN = `## Fleet at a glance

The fleet spent **$4,182 yesterday**, up 6.4% on the trailing 7-day average. Six clusters, 502 nodes, 12,480 pods. The increase is not organic traffic — it traces to two workloads, both of which have concrete fixes below.

The single largest line item remains the **A100 pool** on \`aks-westeu-prod-01\`. Eight GPUs allocated to \`ml-inference/model-server-a100\`, measured utilization 12% over the last 24h. That is roughly **$18.2k/month** of accelerator sitting warm and idle. A guarded ceiling is drafted below; it alerts first and never touches the pods without a human arming it.

## Where the money moved

CPU overcommit is the second theme. \`retrieval/vectordb-ingress\` on \`eks-use1-prod\` requests **16 cores and uses 0.41** at p95. The rightsizing policy below runs in \`recommend\` mode — it computes the ideal request and surfaces it in the CLI and dashboard, but mutates nothing.

Batch is quieter but not clean. The \`gke-euw4-batch\` nodepool ran at 31% average allocation overnight while \`data/etl-nightly\` held 32-core limits it burst into for 11 minutes. Consolidating the pool after the nightly window would recover about **$6.1k/month** with no impact on the job's completion time.

Chargeback is stable everywhere else: the \`edge\` and \`platform\` namespaces are within 3% of their trailing average, and the two staging clusters are flat. No budget policy is near its ceiling — \`prod-monthly-ceiling\` sits at 71% with nine days left in the period.

## Anomalies worth a look

Two behavioral anomalies surfaced overnight. Neither is burning significant money yet, but one of them looks like a bug rather than a sizing problem, so it gets an *investigate* action instead of a rightsize.

- \`platform/metrics-scraper\` on \`aks-ne-staging\` is running **12 replicas where 2 carry the load**. Replica count doubled twice in the last week without a matching config change — worth understanding *why* before rightsizing it.
- \`edge/frontend-gateway\` restarts ticked up (14 in 24h, previously ~2/day). Not costing money yet; flagged for observation, no action drafted.

## Recommended sequence

1. Arm the **GPU idle ceiling** — biggest dollar impact, alert-first escalation, 1h cooldown.
2. Apply the **vectordb-ingress rightsizing** recommendation after one review — the p95 history is 30 days deep and stable.
3. Schedule the **batch nodepool consolidation** for the 02:00–06:00 UTC window.
4. Investigate **metrics-scraper** before touching its replica count.

Taken together the four actions put **$36.8k/month** back on the table, with the first two accounting for 73% of it. All the dollar figures use your negotiated rates from the pricing service, not list price.

Every action ships as a guarded CRD. Nothing here applies itself — you \`kubectl apply\`, the operator observes, and each policy waits for a human to arm it before any mutation.`;

const SPOKEN_SCRIPT =
  "Good morning. Here's your fleet in sixty seconds. " +
  "Yesterday cost four thousand one hundred eighty-two dollars, up about six percent on the week — and the increase is two workloads, not traffic. " +
  "First, the big one: the A100 pool on the Azure production cluster is twelve percent utilized. That's eighteen thousand a month of idle GPU. " +
  "I've drafted a ceiling policy that alerts first and caps only after a human arms it. " +
  "Second, vectordb-ingress on EKS requests sixteen cores and uses less than half of one. A recommend-only rightsizing policy is ready; applying its suggestion saves about eight and a half thousand a month. " +
  "Third, the GKE batch pool can consolidate after the nightly ETL window for another six thousand. " +
  "And one caution: metrics-scraper on staging doubled its replicas twice this week with no config change. Investigate that one before rightsizing it. " +
  "Total on the table: roughly thirty-six thousand dollars a month. All four actions are guarded CRDs — nothing applies itself. Have a good shift.";

export const DEMO_BRIEFING: AdvisorBriefingDTO = {
  id: "brief-demo-2026-07-09",
  generatedAtUnix: GENERATED_AT_UNIX,
  headline:
    "Idle A100s and a 16-core overcommit are 73% of yesterday's waste — $36.8k/mo recoverable across four guarded actions.",
  markdown: MARKDOWN,
  spokenScript: SPOKEN_SCRIPT,
  source: "demo",
  actions: [
    {
      id: "act-demo-gpu-ceiling",
      title: "Arm a GPU idle ceiling on the A100 inference pool",
      impactMonthlyUsd: 18200,
      risk: "medium",
      kind: "ceiling.arm",
      target: "aks-westeu-prod-01/ml-inference/model-server-a100",
      rationale:
        "8× A100 allocated, 12% measured utilization over 24h (~$32/hr burning per idle GPU). The ceiling alerts ml-ops first, then caps the HPA at 40% — it never evicts, and humanArm means nothing fires until you arm it.",
      crdYaml: `apiVersion: kubehero.kubehero.io/v1
kind: CeilingPolicy
metadata:
  name: gpu-inference-idle-cap
  namespace: kubehero-system
spec:
  budgetRef: prod-monthly-ceiling
  trigger:
    burnRateMilli: 1500   # 1.5x budgeted run-rate
    window: "4h"
  escalation:
    - action: alert
      channels: ["slack://ml-ops"]
    - action: hpa.cap
      ratioPercent: 40
      waitAfter: "30m"
  cooldown: "1h"
  humanArm: true`,
      status: "proposed",
    },
    {
      id: "act-demo-rightsize-vectordb",
      title: "Rightsize vectordb-ingress CPU requests (16 → 1 core)",
      impactMonthlyUsd: 8600,
      risk: "low",
      kind: "rightsize.requests",
      target: "eks-use1-prod/retrieval/vectordb-ingress",
      rationale:
        "Requests 16 cores, p95 usage 0.41 over a stable 30-day window. Policy runs in recommend mode — the operator computes the ideal request and surfaces it, but never mutates the Deployment. 50% p95 headroom is preserved.",
      crdYaml: `apiVersion: kubehero.kubehero.io/v1
kind: RightsizingPolicy
metadata:
  name: retrieval-vectordb-recommend
  namespace: kubehero-system
spec:
  scope:
    namespaceSelector:
      matchLabels: { team: retrieval }
  mode: recommend
  targetUtilization: 70
  safety:
    minReplicas: 2
    p95HeadroomPct: 50
    observationWindow: "30d"`,
      status: "proposed",
    },
    {
      id: "act-demo-consolidate-batch",
      title: "Consolidate the gke-euw4-batch nodepool after the ETL window",
      impactMonthlyUsd: 6100,
      risk: "medium",
      kind: "nodepool.consolidate",
      target: "gke-euw4-batch/data/etl-nightly",
      rationale:
        "Pool averages 31% allocation outside the 02:00–06:00 UTC ETL window; etl-nightly holds 32-core limits it bursts into for ~11 minutes. Cordon-based escalation drains only after alert + wait, and the 2x burn-rate trigger keeps it from firing mid-job.",
      crdYaml: `apiVersion: kubehero.kubehero.io/v1
kind: CeilingPolicy
metadata:
  name: batch-pool-consolidate
  namespace: kubehero-system
spec:
  budgetRef: prod-monthly-ceiling
  trigger:
    burnRateMilli: 2000   # 2.0x
    window: "5m"
  escalation:
    - action: alert
      channels: ["slack://data-eng"]
      waitAfter: "30s"
    - action: nodepool.cordon
      waitAfter: "3m"
  cooldown: "10m"
  humanArm: true`,
      status: "proposed",
    },
    {
      id: "act-demo-investigate-scraper",
      title: "Investigate metrics-scraper replica growth before rightsizing",
      impactMonthlyUsd: 3900,
      risk: "high",
      kind: "workload.investigate",
      target: "aks-ne-staging/platform/metrics-scraper",
      rationale:
        "12 replicas where 2 carry the load, and the count doubled twice this week with no matching config change — that pattern sometimes means a scrape-loop bug or an HPA fighting a badly-set metric. The drafted budget alert watches the namespace while you investigate; it takes no automated action.",
      crdYaml: `apiVersion: kubehero.kubehero.io/v1
kind: BudgetPolicy
metadata:
  name: staging-platform-watch
  namespace: kubehero-system
spec:
  scope:
    clusterSelector:
      matchLabels: { env: staging }
  ceiling: "$4000/mo"
  hardStop: false
  humanArm: true
  escalation:
    - action: alert
      channels: ["slack://platform-oncall"]
      waitAfter: "30s"
  alertChannels:
    - "slack://platform-oncall"`,
      status: "proposed",
    },
  ],
};
