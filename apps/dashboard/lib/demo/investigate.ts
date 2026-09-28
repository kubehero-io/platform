// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo investigation agent for /ask. Mirrors the advisor's rules brain:
// detect the question's intent, "call" the same read-only tools the real
// agent uses — against the demo world, so every number matches what the
// explorers show — and write a grounded answer with evidence deep links
// and guarded proposals. Deterministic; labelled source "demo".

import type { Investigation, Step } from "@/lib/advisor/investigate";
import type { AdvisorActionDTO } from "@/lib/api/types";
import { DEMO_BRIEFING } from "@/lib/advisor-demo";
import { rightsizingPolicyYaml } from "@/lib/rightsizing/policy";
import { formatBytes, formatCores, formatUsd } from "@/lib/chart/scale";
import { logqlSelector, logsHref, workloadHref } from "@/lib/url";
import { DEMO_ANOMALIES } from "./anomalies";
import { demoRightsizing, recommend } from "./cost";
import { demoFlamegraph } from "./profiles";
import { demoLogPatterns } from "./logs";
import { demoNetworkCosts, demoServiceMap } from "./network";
import { demoTestRule, demoRules } from "./alerts";
import { findWorkload } from "./world";

export type Intent = "errors" | "spend" | "latency" | "network" | "oom" | "savings" | "overview";

const INTENTS: [Intent, RegExp][] = [
  ["oom", /\b(oom|out of memory|memory|killed|crash)/i],
  ["errors", /\b(fail|error|5\d\d|timeout|timing out|down|incident|broken|payments?)/i],
  ["latency", /\b(slow|latency|cpu|regress|faster|profile|hot ?path)/i],
  ["network", /\b(network|egress|cross-zone|cross zone|bandwidth|traffic|nat)/i],
  ["savings", /\b(save|saving|rightsiz|waste|cheaper|reduce|cut)/i],
  ["spend", /\b(spend|cost|bill|expensive|drove|budget|\$)/i],
];

export function detectIntent(q: string): Intent {
  for (const [intent, re] of INTENTS) if (re.test(q)) return intent;
  return "overview";
}

type Script = { steps: Step[]; result: Omit<Investigation, "steps" | "generatedAtUnix" | "id" | "source"> };

const step = (tool: string, input: unknown, summary: string, durationMs: number): Step => ({
  tool,
  input: JSON.stringify(input),
  summary,
  durationMs,
  error: false,
});

function pct(x: number): string {
  return `${Math.round(x * 100)}%`;
}

function scriptErrors(now: number): Script {
  const rule = demoRules().find((r) => r.id === "rule-payments-timeouts")!;
  const t = demoTestRule(rule, now);
  const timeouts = Math.round(t.results[0]?.value ?? 0);
  const pats = demoLogPatterns('{namespace="shop", level=~"error|fatal"}', { startMs: now - 3_600_000, endMs: now, limit: 5, now });
  const top = pats.patterns[0];
  const map = demoServiceMap({ clusterId: "eks-use1-prod", startMs: now - 3_600_000, endMs: now });
  const stripe = map.edges.find((e) => e.target === "external:api.stripe.com");
  const retrans = stripe ? stripe.retransmits / Math.max(1, stripe.bytes / 1460) : 0;
  const q = `${logqlSelector({ namespace: "shop", workload: "payments" })} |= "timed out"`;
  return {
    steps: [
      step("list_alerts", { state: "firing" }, "2 firing in shop: Payments timeouts (critical), Error log spike on checkout", 142),
      step("query_logs", { query: 'sum by (workload) (count_over_time({namespace="shop", level=~"error|fatal"}[5m]))' }, `payments ${timeouts} timeouts/5m; checkout errors follow the same curve`, 388),
      step("get_log_patterns", { query: '{namespace="shop", level=~"error|fatal"}', window: "1h" }, `top pattern (${top ? top.sharePct.toFixed(0) : "?"}% of errors): stripe POST /v1/payment_intents timed out after 5000ms`, 611),
      step("get_service_map", { cluster: "eks-use1-prod", namespace: "shop" }, `payments → api.stripe.com: ${pct(retrans)} TCP retransmits (fleet baseline 0.05%)`, 274),
      step("list_network_costs", { cluster: "eks-use1-prod" }, "no change in payments egress volume — the requests leave, the answers are late", 196),
    ],
    result: {
      answerMarkdown: `## What is happening

**Payments are failing because calls to Stripe are timing out.** \`shop/payments\` has logged **${timeouts} timeouts in the last 5 minutes** (the rule threshold is 20), all of the form *stripe: request to POST /v1/payment_intents timed out after 5000ms*. It started about 70 minutes ago and has not recovered.

## Why checkout fails too

\`shop/checkout\` calls payments synchronously with its own 5s budget, so every Stripe timeout becomes a **502 "payment authorization failed"** at checkout — the second firing alert is the same incident, not a separate problem.

## Where it breaks

The eBPF flow data shows **${pct(retrans)} TCP retransmits** on the payments → api.stripe.com link against a 0.05% fleet baseline, while request volume is flat. That points at the path out of the cluster (NAT gateway or Stripe's edge), not at payments' own CPU or memory — both are well within requests.

## What to do now

1. Check Stripe's status page and the NAT gateway metrics for us-east-1b.
2. Lower the payments client timeout to 2s with one retry so checkout fails fast instead of holding connections for 5s.
3. Keep the silence off — both alerts are real.`,
      spokenSummary: `Payments are failing because calls to Stripe are timing out: ${timeouts} timeouts in the last five minutes, for about seventy minutes now. Checkout's 502s are the same incident. Network data shows heavy retransmits on the link to Stripe while traffic is flat, so look at the NAT gateway or Stripe's status first, and shorten the client timeout so checkout fails fast.`,
      evidence: [
        { kind: "alert", title: "Payments timeouts — firing", detail: `${timeouts} in 5m · threshold 20`, linkPath: "/alerts", query: rule.query },
        { kind: "logs", title: "Stripe timeout lines", detail: "stripe: request to POST /v1/payment_intents timed out after 5000ms", linkPath: logsHref(q, { window: "6h" }), query: q },
        { kind: "logs", title: "Error patterns in shop", detail: top ? `${top.count.toLocaleString("en-US")} lines · ${top.sharePct.toFixed(0)}% of errors` : "", linkPath: logsHref('{namespace="shop", level=~"error|fatal"}', { tab: "patterns", window: "6h" }), query: '{namespace="shop", level=~"error|fatal"}' },
        { kind: "network", title: "payments → api.stripe.com", detail: `${pct(retrans)} retransmits · flat volume`, linkPath: "/network?cluster=eks-use1-prod&namespace=shop", query: "" },
      ],
      actions: [
        {
          id: "act-ask-payments-investigate",
          title: "Investigate the payments → Stripe egress path (NAT, timeouts)",
          impactMonthlyUsd: 0,
          risk: "low",
          kind: "workload.investigate",
          target: "eks-use1-prod/shop/payments",
          rationale: "Retransmits on the egress link with flat volume point outside the pod. No policy change is proposed — this needs a human to check the NAT gateway and Stripe status.",
          crdYaml: "",
          status: "proposed",
        },
      ],
    },
  };
}

function scriptSpend(): Script {
  const a = DEMO_ANOMALIES.find((x) => x.kind === "spend")!;
  const w = findWorkload("aks-westeu-prod-01", "ml-inference", "model-server-a100")!;
  const gpuAction = DEMO_BRIEFING.actions.find((x) => x.kind === "ceiling.arm")!;
  return {
    steps: [
      step("list_anomalies", { window: "7d", kind: "spend" }, `${a.title} · ${formatUsd(a.impactUsdMonth)}/mo impact`, 164),
      step("get_cost_timeseries", { groupBy: "workload", filters: { namespace: "ml-inference" }, window: "30d" }, "step change 4 days ago on model-server-a100; embedding-batcher flat", 402),
      step("get_cost_allocation", { aggregate: ["workload"], filters: { namespace: "ml-inference" }, window: "7d" }, `model-server-a100: ${formatUsd(w.gpuUsd)}/mo of GPU at ${pct(0.12)} utilisation`, 355),
      step("list_rightsizing", { namespace: "ml-inference" }, `triton requests 8 cores, p95 ${formatCores(w.containers[0].cpuP95)} — CPU is not the driver`, 290),
    ],
    result: {
      answerMarkdown: `## The short answer

**ml-inference spend is up ${a.deltaPct}% week over week, and almost all of it is GPU time on \`model-server-a100\`.** The step change landed four days ago; the replica count and A100 allocation went up while measured GPU utilisation stayed at **~12%**.

## Where the money goes

- \`model-server-a100\` holds **8× A100** (${formatUsd(w.gpuUsd)}/mo of GPU) and uses about an eighth of it.
- CPU is not the story: triton requests 8 cores and runs at a p95 of ${formatCores(w.containers[0].cpuP95)}.
- \`embedding-batcher\` is flat.

## What to do

Arm the GPU idle ceiling below. It alerts first and caps the HPA at 40% only after a human arms it — nothing changes by applying the YAML.`,
      spokenSummary: `ml-inference spend is up thirty-four percent on the week, and it's GPU time on model-server-a100: eight A100s at about twelve percent utilisation since a change four days ago. Arming the GPU idle ceiling would recover most of the eighteen thousand a month; it alerts first and never acts without a human.`,
      evidence: [
        { kind: "anomaly", title: a.title, detail: a.detail, linkPath: "/overview", query: "" },
        { kind: "cost", title: "ml-inference by workload", detail: `model-server-a100 ${formatUsd(w.gpuUsd + w.cpuUsd + w.ramUsd)}/mo`, linkPath: "/allocation?agg=workload&namespace=ml-inference", query: "" },
        { kind: "rightsizing", title: "model-server-a100 recommendation", detail: "GPU idle is the recoverable part", linkPath: workloadHref(w.cluster, w.namespace, w.name), query: "" },
      ],
      actions: [gpuAction],
    },
  };
}

function scriptLatency(now: number): Script {
  const fg = demoFlamegraph({ service: "checkout", namespace: "shop", startMs: now - 3_600_000, endMs: now, diff: true })!;
  const m = fg.nodes.find((n) => n.name === "encoding/json.Marshal")!;
  const share = m.total / fg.total;
  const base = m.baselineTotal / fg.baselineTotal;
  const gz = fg.nodes.find((n) => n.name === "compress/gzip.(*Writer).Write");
  const cost = (fg.costUsdMonth * m.total) / fg.total;
  return {
    steps: [
      step("list_profile_targets", { namespace: "shop" }, "checkout profiled via pprof-scrape, 8 replicas", 120),
      step("get_flamegraph_summary", { service: "checkout", type: "cpu", baseline: "24h" }, `encoding/json.Marshal ${pct(share)} of CPU (was ${pct(base)} yesterday)`, 734),
      step("get_top_functions", { service: "checkout", orderBy: "self" }, "json.structEncoder.encode is the top self-time function", 318),
      step("query_logs", { query: '{namespace="shop", workload="checkout"} |= "POST /api/checkout"' }, "p95 request time up ~60ms since the last deploy", 402),
    ],
    result: {
      answerMarkdown: `## The short answer

**checkout got slower because it now spends ${pct(share)} of its CPU in \`encoding/json.Marshal\` — up from ${pct(base)} yesterday** (+${pct(share / base - 1)} relative). The hot path is \`main.(*API).priceCart → encoding/json.Marshal → structEncoder.encode\`, i.e. the priced cart is being serialised on every request.

## What it costs

That path alone is worth about **${formatUsd(cost)}/mo** of checkout's CPU. ${gz ? "Gzip got cheaper at the same time (a compression level change), which hides part of the regression in the totals." : ""}

## What to do

Cache the marshalled price quote per cart version, or switch that struct to a hand-written encoder. Open the diff flamegraph below and search for \`json\` to see exactly which frames grew.`,
      spokenSummary: `Checkout is slower because it now spends much more CPU marshalling the priced cart to JSON on every request — up about forty percent since yesterday. Caching the serialised quote would take most of it back.`,
      evidence: [
        { kind: "profile", title: "checkout CPU diff vs yesterday", detail: `json.Marshal ${pct(base)} → ${pct(share)}`, linkPath: "/profiles?service=checkout&namespace=shop&diff=1&baseline=1d&fn=encoding%2Fjson.Marshal", query: "" },
        { kind: "profile", title: "Top functions · checkout", detail: "structEncoder.encode leads self time", linkPath: "/profiles?service=checkout&namespace=shop", query: "" },
      ],
      actions: [],
    },
  };
}

function scriptNetwork(now: number): Script {
  const c = demoNetworkCosts({ clusterId: "eks-use1-prod", startMs: now - 86_400_000, endMs: now });
  const top = c.costs.slice(0, 3);
  return {
    steps: [
      step("list_network_costs", { cluster: "eks-use1-prod", limit: 10 }, `${formatUsd(c.totalUsdMonth)}/mo total; top: ${top.map((x) => x.workload).join(", ")}`, 233),
      step("get_service_map", { cluster: "eks-use1-prod" }, "storefront → internet and catalog → S3 dominate; postgres replicates across 3 zones", 512),
    ],
    result: {
      answerMarkdown: `## Network spend in eks-use1-prod: ${formatUsd(c.totalUsdMonth)}/mo

${top.map((x, i) => `${i + 1}. **${x.namespace}/${x.workload}** — ${formatUsd(x.totalUsdMonth)}/mo (egress ${formatUsd(x.egressUsdMonth)}, cross-zone ${formatUsd(x.crossZoneUsdMonth)}); top destination ${x.topDestination || "—"}.`).join("\n")}

## What to do

- Put a CDN in front of storefront's static responses — that is most of the internet egress.
- Route catalog's S3 image reads through a **gateway VPC endpoint**: same-region S3 traffic through an endpoint is free, through the NAT it is billed as egress.
- Keep postgres replication cross-zone (that's the point of it), but pin read traffic to same-zone replicas.`,
      spokenSummary: `Network costs about ${Math.round(c.totalUsdMonth / 1000)} thousand dollars a month in the main cluster. Storefront's responses to the internet and catalog's S3 reads through the NAT are most of it — a CDN and an S3 gateway endpoint would cut it sharply.`,
      evidence: top.map((x) => ({
        kind: "network",
        title: `${x.namespace}/${x.workload}`,
        detail: `${formatUsd(x.totalUsdMonth)}/mo · → ${x.topDestination}`,
        linkPath: "/network?cluster=eks-use1-prod",
        query: "",
      })),
      actions: [],
    },
  };
}

function scriptOom(): Script {
  const w = findWorkload("eks-use1-prod", "shop", "cart")!;
  const r = recommend(w, w.containers[0]);
  const yaml = rightsizingPolicyYaml({
    cluster: w.cluster,
    namespace: w.namespace,
    workload: w.name,
    container: r.container,
    window: "7d",
    headroomPct: 15,
    replicas: w.replicas,
    siblings: ["storefront", "checkout", "payments", "catalog", "postgres", "redis"],
    cpuRecommended: formatCores(r.cpu.recommended),
    memRecommended: formatBytes(r.mem.recommended),
    savingsUsdMonth: r.savingsUsdMonth,
  });
  const action: AdvisorActionDTO = {
    id: "act-ask-cart-memory",
    title: `Raise cart memory to ${formatBytes(r.mem.recommended)} (OOM guard)`,
    impactMonthlyUsd: Math.max(0, r.savingsUsdMonth),
    risk: "low",
    kind: "rightsize.requests",
    target: `${w.cluster}/${w.namespace}/${w.name}`,
    rationale: `${r.oomKills} OOM kills in 7 days with a 512 MiB limit and a max working set at the limit. Memory never shrinks for an OOM-ing workload; this raises it 25% past the old limit.`,
    crdYaml: yaml,
    status: "proposed",
  };
  return {
    steps: [
      step("list_alerts", { rule: "OOM kills" }, "cart OOM-killed twice in the last 10 minutes", 118),
      step("list_rightsizing", { namespace: "shop", workload: "cart" }, `${r.oomKills} OOM kills / 7d · memory max ${formatBytes(r.mem.max)} = limit`, 301),
      step("query_logs", { query: '{namespace="shop", workload="cart", level="fatal"}' }, "runtime: out of memory: cannot allocate 268435456-byte block", 344),
    ],
    result: {
      answerMarkdown: `## The short answer

**cart is being OOM-killed because its working set reaches its 512 MiB limit.** Seven OOM kills in the observation window; the Go runtime logs *out of memory: cannot allocate … (536 MB in use)* right before each kill, and GC pauses climb as the heap nears the limit.

## What to do

Raise memory to **${formatBytes(r.mem.recommended)}** — the rightsizing engine never shrinks memory for a workload that OOMs, and sizes to 25% past the old limit. The policy below does exactly that through the guarded flow.`,
      spokenSummary: `Cart runs out of memory at its five hundred twelve megabyte limit — seven kills this week. Raising the request and limit to about six hundred forty megabytes stops it; the policy is ready and waits for a human to arm it.`,
      evidence: [
        { kind: "alert", title: "OOM kills — firing", detail: "cart · 2 in 10m", linkPath: "/alerts", query: 'events{kind="oom_killed"}[10m]' },
        { kind: "rightsizing", title: "cart · memory upsize", detail: `${formatBytes(r.mem.request)} → ${formatBytes(r.mem.recommended)}`, linkPath: `/rightsizing?rec=${encodeURIComponent(r.id)}`, query: "" },
        { kind: "logs", title: "fatal: out of memory", detail: "runtime: out of memory: cannot allocate …", linkPath: logsHref(logqlSelector({ namespace: "shop", workload: "cart", level: "fatal" }), { window: "24h" }), query: "" },
      ],
      actions: [action],
    },
  };
}

function scriptSavings(): Script {
  const rs = demoRightsizing();
  const topRecs = rs.recs.filter((r) => r.direction === "downsize" && r.confidence === "high").slice(0, 3);
  const gpu = DEMO_BRIEFING.actions.find((x) => x.kind === "ceiling.arm")!;
  const vec = DEMO_BRIEFING.actions.find((x) => x.kind === "rightsize.requests")!;
  return {
    steps: [
      step("list_rightsizing", { minSavingsUsdMonth: 500, confidence: "high" }, `${rs.recs.length} recommendations · ${formatUsd(rs.totalSavingsUsdMonth)}/mo total`, 412),
      step("get_efficiency", { window: "7d" }, "fleet CPU efficiency 31%, idle capacity 22% of spend", 188),
      step("list_anomalies", { window: "7d" }, "ml-inference GPU idle is the largest single line", 150),
    ],
    result: {
      answerMarkdown: `## ${formatUsd(topRecs.reduce((s, r) => s + r.savingsUsdMonth, 0) + gpu.impactMonthlyUsd)}/mo with low risk

1. **Arm the GPU idle ceiling on model-server-a100** — ${formatUsd(gpu.impactMonthlyUsd)}/mo. Eight A100s at ~12% utilisation.
${topRecs.map((r, i) => `${i + 2}. **Rightsize ${r.namespace}/${r.workload}** — ${formatUsd(r.savingsUsdMonth)}/mo. CPU ${formatCores(r.cpu.request)} → ${formatCores(r.cpu.recommended)}, high confidence.`).join("\n")}

Every item is a guarded policy: applying the YAML changes nothing until a human arms it, and the operator re-checks confidence, OOM history and rollout state before touching requests.`,
      spokenSummary: `The cheapest safe wins: arm the GPU idle ceiling on the A100 pool, then rightsize the three high-confidence over-requesters. Together that's well over ten thousand dollars a month, and nothing changes until someone arms each policy.`,
      evidence: [
        { kind: "rightsizing", title: "High-confidence downsizes", detail: `${topRecs.length} workloads`, linkPath: "/rightsizing?conf=high&direction=downsize", query: "" },
        { kind: "cost", title: "Idle GPU on ml-inference", detail: "8× A100 at 12%", linkPath: "/allocation?agg=workload&namespace=ml-inference", query: "" },
      ],
      actions: [gpu, vec],
    },
  };
}

function scriptOverview(): Script {
  return {
    steps: [
      step("get_efficiency", { window: "7d" }, "fleet efficiency score 24/100", 176),
      step("list_alerts", { state: "firing" }, "5 firing: payments timeouts, checkout errors, cart OOM, 2 spend anomalies", 131),
      step("list_anomalies", { window: "7d" }, "ml-inference +34% w/w is the biggest cost mover", 144),
    ],
    result: {
      answerMarkdown: `## Fleet right now

- **Incident:** shop/payments → Stripe timeouts for ~70 minutes; checkout returns 502s as a result. Ask *"why are checkout payments failing?"* for the full trace.
- **Reliability:** cart is OOM-killing at its 512 MiB limit — a memory upsize is ready.
- **Cost:** ml-inference is up 34% on idle A100s; vectordb-ingress requests 16 cores and uses 0.4.

Pick one of the suggested questions to go deeper — every answer links to the logs, profiles, flows and costs it is based on.`,
      spokenSummary: "One live incident on payments, one memory problem on cart, and the biggest cost mover is idle GPUs on ml-inference. Ask about any of them for the details.",
      evidence: [
        { kind: "alert", title: "5 alerts firing", detail: "payments, checkout, cart, ml-inference, data", linkPath: "/alerts", query: "" },
        { kind: "anomaly", title: "ml-inference spend +34% w/w", detail: "GPU idle", linkPath: "/overview", query: "" },
      ],
      actions: [],
    },
  };
}

export function demoInvestigation(question: string, now = Date.now()): { steps: Step[]; progress: string[]; result: Investigation } {
  const intent = detectIntent(question);
  const s =
    intent === "errors"
      ? scriptErrors(now)
      : intent === "spend"
        ? scriptSpend()
        : intent === "latency"
          ? scriptLatency(now)
          : intent === "network"
            ? scriptNetwork(now)
            : intent === "oom"
              ? scriptOom()
              : intent === "savings"
                ? scriptSavings()
                : scriptOverview();
  const id = `inv-demo-${intent}`;
  const topic: Record<Intent, string> = {
    errors: "an error / incident question",
    spend: "a spend question",
    latency: "a latency question",
    network: "a network cost question",
    oom: "a memory question",
    savings: "a savings question",
    overview: "a fleet overview",
  };
  return {
    steps: s.steps,
    progress: ["reading the question", `looks like ${topic[intent]} — picking tools`, "writing the answer from the evidence"],
    result: { ...s.result, id, steps: s.steps, source: "demo", generatedAtUnix: Math.floor(now / 1000) },
  };
}
