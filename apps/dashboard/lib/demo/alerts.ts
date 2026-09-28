// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo AlertsService: the default rule set the control plane seeds on
// first boot plus a few custom rules, alert state that matches the rest
// of the demo world (the payments incident on /logs, cart OOM kills on
// /rightsizing, the ml-inference spend anomaly on the overview), one
// active silence, and a TestAlertRule evaluator for every rule kind so
// the editor's "Test" button returns real numbers in demo mode.

import type { Alert, AlertRule, Evaluation, RuleKind, Silence, TestResult } from "@/lib/alerts/types";
import { parseSignalSelector } from "@/lib/alerts/selector";
import { LogQLError } from "@/lib/logql/parse";
import { logqlSelector, logsHref } from "@/lib/url";
import { DEMO_ANOMALIES } from "./anomalies";
import { anchorOf, demoQueryLogs } from "./logs";
import { demoNetworkCosts } from "./network";
import { demoWorkloads, totalUsd } from "./world";

const MIN = 60_000;
const HOUR = 60 * MIN;

const iso = (ms: number) => new Date(ms).toISOString().replace(/\.\d{3}Z$/, "Z");

export function demoRules(): AlertRule[] {
  const base = { createdAt: "2026-09-01T09:00:00Z", updatedAt: "2026-09-21T14:12:00Z", createdBy: "kubehero (default rule)", evalInterval: "1m", enabled: true, labels: {} as Record<string, string> };
  return [
    {
      ...base,
      id: "rule-error-spike",
      name: "Error log spike",
      description: "More than 100 error/fatal lines in 5 minutes from one workload.",
      kind: "logs",
      query: 'sum by (cluster, namespace, workload) (count_over_time({level=~"error|fatal"}[5m]))',
      op: ">",
      threshold: 100,
      pendingFor: "5m",
      severity: "critical",
      channels: ["slack://ops-oncall"],
      annotations: { summary: "{{ $labels.workload }} logged {{ $value }} errors in 5m", runbook_url: "https://kubehero.io/docs/runbooks/error-spike" },
    },
    {
      ...base,
      id: "rule-oom",
      name: "OOM kills",
      description: "Any container killed for exceeding its memory limit.",
      kind: "event",
      query: 'events{kind="oom_killed"}[10m]',
      op: ">",
      threshold: 0,
      pendingFor: "0s",
      severity: "critical",
      channels: ["slack://ops-oncall"],
      annotations: { summary: "{{ $labels.workload }} OOM-killed {{ $value }}× in 10m — see /rightsizing" },
    },
    {
      ...base,
      id: "rule-spend-anomaly",
      name: "Spend anomaly > $1k/mo",
      description: "A statistically unusual change in spend worth more than $1k a month.",
      kind: "anomaly",
      query: 'anomaly{kind="spend"}',
      op: ">",
      threshold: 1000,
      pendingFor: "0s",
      severity: "warn",
      channels: ["slack://finops"],
      annotations: { summary: "{{ $labels.subject }} spend moved — ${{ $value }}/mo impact" },
    },
    {
      ...base,
      id: "rule-budget-burn",
      name: "Budget burn > 1.5×",
      description: "The production budget is burning 50% faster than planned.",
      kind: "budget",
      query: "prod-monthly-ceiling",
      op: ">",
      threshold: 1.5,
      pendingFor: "30m",
      severity: "critical",
      channels: ["pagerduty://finops-oncall", "slack://finops"],
      annotations: { summary: "prod-monthly-ceiling burning at {{ $value }}× budget" },
    },
    {
      ...base,
      id: "rule-egress",
      name: "Egress > $5/h",
      description: "Internet egress + cross-zone spend above $5/hour for a workload.",
      kind: "network",
      query: 'network{cluster="eks-use1-prod"} by (workload)',
      op: ">",
      threshold: 5,
      pendingFor: "15m",
      severity: "warn",
      channels: ["slack://platform"],
      annotations: { summary: "{{ $labels.workload }} network spend at ${{ $value }}/h" },
    },
    {
      ...base,
      id: "rule-payments-timeouts",
      name: "Payments timeouts",
      description: "Stripe calls timing out from shop/payments.",
      kind: "logs",
      query: 'sum by (workload) (count_over_time({namespace="shop", workload="payments"} |= "timed out" [5m]))',
      op: ">",
      threshold: 20,
      pendingFor: "5m",
      severity: "critical",
      channels: ["slack://payments-team", "pagerduty://payments"],
      createdBy: "maria@acme.io",
      annotations: { summary: "{{ $value }} Stripe timeouts in 5m", runbook_url: "https://wiki.acme.io/payments/stripe-timeouts" },
    },
    {
      ...base,
      id: "rule-gpu-spend",
      name: "ml-inference spend > $40/h",
      description: "Guard rail on the A100 pool while the idle-GPU ceiling is tuned.",
      kind: "cost",
      query: 'cost{namespace="ml-inference"}',
      op: ">",
      threshold: 40,
      pendingFor: "30m",
      severity: "warn",
      channels: ["slack://ml-ops"],
      enabled: false,
      createdBy: "li@acme.io",
      annotations: { summary: "ml-inference spending ${{ $value }}/h" },
    },
  ];
}

export function demoAlerts(now = Date.now()): Alert[] {
  const anchor = anchorOf(now);
  const payments = evalLogsAt('sum by (workload) (count_over_time({namespace="shop", workload="payments"} |= "timed out" [5m]))', now);
  const paymentsValue = payments[0]?.value ?? 180;
  const cartPod = "cart-7f9c4b8d5-" + "x2k9q";
  const alerts: Alert[] = [
    {
      id: "alert-payments-timeouts",
      ruleId: "rule-payments-timeouts",
      ruleName: "Payments timeouts",
      kind: "logs",
      state: "firing",
      severity: "critical",
      labels: { cluster: "eks-use1-prod", namespace: "shop", workload: "payments" },
      value: Math.round(paymentsValue),
      summary: `${Math.round(paymentsValue)} Stripe timeouts in 5m`,
      description: "Stripe calls timing out from shop/payments.",
      startedAt: iso(anchor - 66 * MIN),
      firedAt: iso(anchor - 61 * MIN),
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: logsHref(`${logqlSelector({ namespace: "shop", workload: "payments" })} |= "timed out"`, { window: "6h" }),
    },
    {
      id: "alert-error-spike-checkout",
      ruleId: "rule-error-spike",
      ruleName: "Error log spike",
      kind: "logs",
      state: "firing",
      severity: "critical",
      labels: { cluster: "eks-use1-prod", namespace: "shop", workload: "checkout" },
      value: 212,
      summary: "checkout logged 212 errors in 5m",
      description: "More than 100 error/fatal lines in 5 minutes from one workload.",
      startedAt: iso(anchor - 58 * MIN),
      firedAt: iso(anchor - 53 * MIN),
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: logsHref(`${logqlSelector({ namespace: "shop", workload: "checkout", level: "error" })}`, { window: "6h" }),
    },
    {
      id: "alert-oom-cart",
      ruleId: "rule-oom",
      ruleName: "OOM kills",
      kind: "event",
      state: "firing",
      severity: "critical",
      labels: { cluster: "eks-use1-prod", namespace: "shop", workload: "cart", pod: cartPod },
      value: 2,
      summary: "cart OOM-killed 2× in 10m — see /rightsizing",
      description: "Any container killed for exceeding its memory limit.",
      startedAt: iso(anchor - 37 * MIN),
      firedAt: iso(anchor - 37 * MIN),
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: "/rightsizing?q=cart",
    },
    {
      id: "alert-anomaly-ml",
      ruleId: "rule-spend-anomaly",
      ruleName: "Spend anomaly > $1k/mo",
      kind: "anomaly",
      state: "firing",
      severity: "warn",
      labels: { subject: "ml-inference", namespace: "ml-inference", anomaly: "anom-7c91" },
      value: 18200,
      summary: "ml-inference spend moved — $18200/mo impact",
      description: "model-server-a100 + retrieval-indexer carry 78% of the increase",
      startedAt: iso(anchor - 26 * HOUR),
      firedAt: iso(anchor - 26 * HOUR),
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: "/allocation?agg=workload&namespace=ml-inference",
    },
    {
      id: "alert-anomaly-data",
      ruleId: "rule-spend-anomaly",
      ruleName: "Spend anomaly > $1k/mo",
      kind: "anomaly",
      state: "firing",
      severity: "warn",
      labels: { subject: "data", namespace: "data" },
      value: 2400,
      summary: "data spend moved — $2400/mo impact",
      description: "etl-nightly backfill doubled nightly runtime",
      startedAt: iso(anchor - 5 * HOUR),
      firedAt: iso(anchor - 5 * HOUR),
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: true,
      linkPath: "/allocation?agg=workload&namespace=data",
    },
    {
      id: "alert-egress-storefront",
      ruleId: "rule-egress",
      ruleName: "Egress > $5/h",
      kind: "network",
      state: "pending",
      severity: "warn",
      labels: { cluster: "eks-use1-prod", workload: "storefront" },
      value: 12.3,
      summary: "storefront network spend at $12.3/h",
      description: "Internet egress + cross-zone spend above $5/hour for a workload.",
      startedAt: iso(anchor - 6 * MIN),
      firedAt: "",
      resolvedAt: "",
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: "/network?cluster=eks-use1-prod&namespace=shop",
    },
    {
      id: "alert-budget-prod",
      ruleId: "rule-budget-burn",
      ruleName: "Budget burn > 1.5×",
      kind: "budget",
      state: "resolved",
      severity: "critical",
      labels: { policy: "prod-monthly-ceiling" },
      value: 1.12,
      summary: "prod-monthly-ceiling burning at 1.62× budget",
      description: "The production budget is burning 50% faster than planned.",
      startedAt: iso(anchor - 9 * HOUR),
      firedAt: iso(anchor - 8.5 * HOUR),
      resolvedAt: iso(anchor - 3 * HOUR),
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: "/budgets",
    },
    {
      id: "alert-error-spike-gateway",
      ruleId: "rule-error-spike",
      ruleName: "Error log spike",
      kind: "logs",
      state: "resolved",
      severity: "critical",
      labels: { cluster: "gke-usc1-prod", namespace: "edge", workload: "frontend-gateway" },
      value: 34,
      summary: "frontend-gateway logged 141 errors in 5m",
      description: "More than 100 error/fatal lines in 5 minutes from one workload.",
      startedAt: iso(anchor - 11 * HOUR),
      firedAt: iso(anchor - 10.9 * HOUR),
      resolvedAt: iso(anchor - 9.8 * HOUR),
      lastEvalAt: iso(now - 20_000),
      silenced: false,
      linkPath: logsHref(logqlSelector({ namespace: "edge", workload: "frontend-gateway", level: "error" }), { window: "24h" }),
    },
  ];
  return alerts;
}

export function demoSilences(now = Date.now(), includeExpired = false): Silence[] {
  const anchor = anchorOf(now);
  const s: Silence[] = [
    {
      id: "sil-7d21",
      matchers: { alertname: "Spend anomaly > $1k/mo", namespace: "data" },
      startsAt: iso(anchor - 2 * HOUR),
      endsAt: iso(anchor + 22 * HOUR),
      createdBy: "ops@acme.io",
      comment: "Backfill running until tomorrow — the spend is expected.",
    },
    {
      id: "sil-51aa",
      matchers: { alertname: "Egress > $5/h", workload: "llm-finetune" },
      startsAt: iso(anchor - 50 * HOUR),
      endsAt: iso(anchor - 26 * HOUR),
      createdBy: "li@acme.io",
      comment: "Model checkpoint upload to S3.",
    },
  ];
  return includeExpired ? s : s.filter((x) => Date.parse(x.endsAt) > now);
}

// ─── TestAlertRule, demo edition ────────────────────────────────────────

function compare(v: number, op: string, t: number): boolean {
  switch (op) {
    case ">":
      return v > t;
    case ">=":
      return v >= t;
    case "<":
      return v < t;
    case "<=":
      return v <= t;
    case "==":
      return v === t;
    case "!=":
      return v !== t;
    default:
      return false;
  }
}

function evalLogsAt(query: string, now: number): Evaluation[] {
  const r = demoQueryLogs(query, { startMs: now - 5 * MIN, endMs: now, limit: 1, direction: "backward", stepMs: 5 * MIN, now });
  if (r.resultType !== "matrix") throw new LogQLError("logs rules need a metric query, e.g. count_over_time(…[5m])", 0);
  return r.series
    .map((s) => ({ labels: s.labels, value: s.points.at(-1)?.v ?? 0, triggered: false }))
    .filter((e) => Number.isFinite(e.value));
}

const BUDGET_BURN: Record<string, number> = {
  "prod-monthly-ceiling": 1.12,
  "ml-gpu-ceiling": 1.64,
  "prod-burn-rate-2x": 0.8,
  "gpu-inference-cap": 2.1,
  "staging-monthly": 0.4,
};

const EVENT_COUNTS: { kind: string; labels: Record<string, string>; perTenMin: number }[] = [
  { kind: "oom_killed", labels: { cluster: "eks-use1-prod", namespace: "shop", workload: "cart", pod: "cart-7f9c4b8d5-x2k9q" }, perTenMin: 2 },
  { kind: "unschedulable", labels: { cluster: "gke-euw4-batch", namespace: "data", workload: "etl-nightly" }, perTenMin: 12 },
  { kind: "crash_loop", labels: { cluster: "gke-usc1-prod", namespace: "edge", workload: "frontend-gateway" }, perTenMin: 1 },
  { kind: "evicted", labels: { cluster: "aks-ne-staging", namespace: "platform", workload: "metrics-scraper" }, perTenMin: 0.5 },
];

function groupBy<T extends { labels: Record<string, string>; value: number }>(rows: T[], by: string[]): Evaluation[] {
  if (by.length === 0) {
    return rows.length === 0 ? [] : [{ labels: {}, value: rows.reduce((s, r) => s + r.value, 0), triggered: false }];
  }
  const m = new Map<string, Evaluation>();
  for (const r of rows) {
    const labels = Object.fromEntries(by.map((k) => [k, r.labels[k] ?? ""]));
    const key = JSON.stringify(labels);
    const e = m.get(key) ?? { labels, value: 0, triggered: false };
    e.value += r.value;
    m.set(key, e);
  }
  return [...m.values()];
}

export function demoTestRule(rule: { kind: RuleKind; query: string; op: string; threshold: number }, now = Date.now()): TestResult {
  const t0 = performance.now();
  let results: Evaluation[] = [];
  try {
    switch (rule.kind) {
      case "logs":
        results = evalLogsAt(rule.query, now);
        break;
      case "cost": {
        const p = parseSignalSelector(rule.query, "cost");
        if (!p.ok) throw new Error(p.error);
        const rows = demoWorkloads()
          .filter((w) => Object.entries(p.sel.matchers).every(([k, v]) => ({ cluster: w.cluster, namespace: w.namespace, workload: w.name, team: w.team, nodepool: w.nodepool } as Record<string, string>)[k] === v))
          .map((w) => ({ labels: { cluster: w.cluster, namespace: w.namespace, workload: w.name, team: w.team, nodepool: w.nodepool }, value: totalUsd(w) / 730 }));
        results = groupBy(rows, p.sel.by);
        break;
      }
      case "network": {
        const p = parseSignalSelector(rule.query, "network");
        if (!p.ok) throw new Error(p.error);
        const cluster = p.sel.matchers.cluster ?? "eks-use1-prod";
        const rows = demoNetworkCosts({ clusterId: cluster, startMs: now - HOUR, endMs: now })
          .costs.filter((c) => (!p.sel.matchers.namespace || c.namespace === p.sel.matchers.namespace) && (!p.sel.matchers.workload || c.workload === p.sel.matchers.workload))
          .map((c) => ({ labels: { cluster, namespace: c.namespace, workload: c.workload }, value: c.totalUsdMonth / 730 }));
        results = groupBy(rows, p.sel.by);
        break;
      }
      case "anomaly": {
        const p = parseSignalSelector(rule.query, "anomaly");
        if (!p.ok) throw new Error(p.error);
        results = DEMO_ANOMALIES.filter((a) => !p.sel.matchers.kind || a.kind === p.sel.matchers.kind).map((a) => ({
          labels: { anomaly: a.id, kind: a.kind, subject: a.subject },
          value: a.impactUsdMonth,
          triggered: false,
        }));
        break;
      }
      case "event": {
        const p = parseSignalSelector(rule.query, "events");
        if (!p.ok) throw new Error(p.error);
        const range = p.sel.rangeMs ?? 10 * MIN;
        const rows = EVENT_COUNTS.filter((e) => e.kind === p.sel.matchers.kind)
          .filter((e) => Object.entries(p.sel.matchers).every(([k, v]) => k === "kind" || e.labels[k] === v))
          .map((e) => ({ labels: e.labels, value: Math.round(e.perTenMin * (range / (10 * MIN))) }));
        results = p.sel.by.length ? groupBy(rows, p.sel.by) : rows.map((r) => ({ ...r, triggered: false }));
        break;
      }
      case "budget": {
        const burn = BUDGET_BURN[rule.query.trim()];
        if (burn === undefined) throw new Error(`no BudgetPolicy named "${rule.query.trim()}"`);
        results = [{ labels: { policy: rule.query.trim() }, value: burn, triggered: false }];
        break;
      }
    }
  } catch (e) {
    return { results: [], error: e instanceof Error ? e.message : String(e), execMs: performance.now() - t0 };
  }
  return {
    results: results
      .map((r) => ({ ...r, value: Math.round(r.value * 1000) / 1000, triggered: compare(r.value, rule.op, rule.threshold) }))
      .sort((a, b) => b.value - a.value)
      .slice(0, 100),
    error: "",
    execMs: performance.now() - t0,
  };
}
