// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Alert rules: wire mapping, editor draft ⇄ rule conversion, and the
// validation the editor runs before a rule ever reaches the control
// plane (which validates again — this is for fast, precise feedback).
// Pure + client-safe.

import { arr, bool, num, rec, str } from "@/lib/api/wire";
import {
  OPS,
  RULE_KINDS,
  SEVERITIES,
  type Alert,
  type AlertJson,
  type AlertRule,
  type AlertRuleJson,
  type AlertSeverity,
  type AlertState,
  type Op,
  type RuleDraft,
  type RuleKind,
  type Silence,
  type SilenceJson,
  type TestAlertRuleResponseJson,
  type TestResult,
} from "./types";

const isKind = (v: string): v is RuleKind => (RULE_KINDS as readonly string[]).includes(v);
const isOp = (v: string): v is Op => (OPS as readonly string[]).includes(v);
const isSev = (v: string): v is AlertSeverity => (SEVERITIES as readonly string[]).includes(v);

export function toRule(r: AlertRuleJson): AlertRule {
  const kind = str(r.kind);
  const op = str(r.op);
  const sev = str(r.severity);
  return {
    id: str(r.id),
    name: str(r.name),
    description: str(r.description),
    kind: isKind(kind) ? kind : "logs",
    query: str(r.query),
    op: isOp(op) ? op : ">",
    threshold: num(r.threshold),
    pendingFor: str(r.pendingFor) || "0s",
    severity: isSev(sev) ? sev : "warn",
    channels: arr(r.channels).filter((c): c is string => typeof c === "string"),
    labels: rec(r.labels),
    annotations: rec(r.annotations),
    enabled: bool(r.enabled),
    evalInterval: str(r.evalInterval) || "1m",
    createdAt: str(r.createdAt),
    updatedAt: str(r.updatedAt),
    createdBy: str(r.createdBy),
  };
}

export function toAlert(a: AlertJson): Alert {
  const state = str(a.state);
  const sev = str(a.severity);
  return {
    id: str(a.id),
    ruleId: str(a.ruleId),
    ruleName: str(a.ruleName) || "(unnamed rule)",
    kind: str(a.kind),
    state: (["pending", "firing", "resolved"].includes(state) ? state : "firing") as AlertState,
    severity: isSev(sev) ? sev : "warn",
    labels: rec(a.labels),
    value: num(a.value),
    summary: str(a.summary),
    description: str(a.description),
    startedAt: str(a.startedAt),
    firedAt: str(a.firedAt),
    resolvedAt: str(a.resolvedAt),
    lastEvalAt: str(a.lastEvalAt),
    silenced: bool(a.silenced),
    linkPath: safeLinkPath(str(a.linkPath)),
  };
}

/** Only same-origin dashboard paths become links (the value comes from rule templates). */
export function safeLinkPath(p: string): string {
  return p.startsWith("/") && !p.startsWith("//") && !p.includes("\\") && p.length <= 1024 ? p : "";
}

export function toSilence(s: SilenceJson): Silence {
  return {
    id: str(s.id),
    matchers: rec(s.matchers),
    startsAt: str(s.startsAt),
    endsAt: str(s.endsAt),
    createdBy: str(s.createdBy),
    comment: str(s.comment),
  };
}

export function toTestResult(r: TestAlertRuleResponseJson): TestResult {
  return {
    results: arr(r.results).map((x) => ({ labels: rec(x.labels), value: num(x.value), triggered: bool(x.triggered) })),
    error: str(r.error),
    execMs: num(r.execMs),
  };
}

// ─── validation ─────────────────────────────────────────────────────────

export const CHANNEL_SCHEMES = ["slack", "pagerduty", "opsgenie", "teams", "teams+https", "webhook+https", "alertmanager+https", "discord+https"] as const;
const DURATION_RE = /^(\d+(ms|s|m|h))+$/;
const LABEL_KEY_RE = /^[a-zA-Z_][a-zA-Z0-9_]{0,63}$/;

export function parseDurationMs(s: string): number | null {
  if (!DURATION_RE.test(s)) return null;
  let total = 0;
  for (const m of s.matchAll(/(\d+)(ms|s|m|h)/g)) total += Number(m[1]) * { ms: 1, s: 1e3, m: 6e4, h: 3.6e6 }[m[2] as "ms"];
  return total;
}

export function validateChannel(c: string): string | null {
  const m = /^([a-z+]+):\/\/(.+)$/.exec(c);
  if (!m) return `"${c}" is not a channel URL (e.g. slack://ops-oncall)`;
  if (!(CHANNEL_SCHEMES as readonly string[]).includes(m[1])) return `unknown channel scheme "${m[1]}://"`;
  if (m[1].endsWith("+https")) {
    try {
      const u = new URL(`https://${m[2]}`);
      if (!u.hostname) return `"${c}" has no host`;
    } catch {
      return `"${c}" is not a valid URL`;
    }
  }
  if (c.length > 512) return "channel URL too long";
  return null;
}

/** "k=v, k2=v2" (or newline separated) → map, with errors. */
export function parseLabels(s: string): { labels: Record<string, string>; error: string | null } {
  const labels: Record<string, string> = {};
  for (const part of s.split(/[\n,]+/).map((x) => x.trim()).filter(Boolean)) {
    const i = part.indexOf("=");
    if (i <= 0) return { labels, error: `"${part}" is not key=value` };
    const k = part.slice(0, i).trim();
    const v = part.slice(i + 1).trim().replace(/^"(.*)"$/, "$1");
    if (!LABEL_KEY_RE.test(k)) return { labels, error: `invalid label name "${k}"` };
    if (v.length > 256) return { labels, error: `value for ${k} is too long` };
    labels[k] = v;
  }
  if (Object.keys(labels).length > 32) return { labels, error: "at most 32 labels" };
  return { labels, error: null };
}

export type DraftErrors = Partial<Record<keyof RuleDraft, string>>;

export function validateDraft(d: RuleDraft): DraftErrors {
  const e: DraftErrors = {};
  if (!d.name.trim()) e.name = "required";
  else if (d.name.length > 128) e.name = "at most 128 characters";
  if (!isKind(d.kind)) e.kind = "unknown kind";
  if (!d.query.trim()) e.query = "required";
  else if (d.query.length > 4096) e.query = "at most 4096 characters";
  if (!isOp(d.op)) e.op = "unknown operator";
  if (d.threshold.trim() === "" || !Number.isFinite(Number(d.threshold))) e.threshold = "a number";
  const pf = parseDurationMs(d.pendingFor || "0s");
  if (pf === null) e.pendingFor = "a duration like 5m";
  else if (pf > 24 * 3.6e6) e.pendingFor = "at most 24h";
  const ei = parseDurationMs(d.evalInterval || "1m");
  if (ei === null) e.evalInterval = "a duration like 1m";
  else if (ei < 15_000) e.evalInterval = "at least 15s";
  if (!isSev(d.severity)) e.severity = "unknown severity";
  const channels = splitChannels(d.channels);
  const bad = channels.map(validateChannel).find((x) => x !== null);
  if (bad) e.channels = bad;
  if (channels.length > 16) e.channels = "at most 16 channels";
  const l = parseLabels(d.labels);
  if (l.error) e.labels = l.error;
  if (d.runbookUrl && !/^https?:\/\/\S+$/.test(d.runbookUrl)) e.runbookUrl = "an http(s) URL";
  if (d.summary.length > 512) e.summary = "at most 512 characters";
  return e;
}

export function splitChannels(s: string): string[] {
  return [...new Set(s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean))];
}

export function draftFromRule(r?: AlertRule): RuleDraft {
  return {
    id: r?.id ?? "",
    name: r?.name ?? "",
    description: r?.description ?? "",
    kind: r?.kind ?? "logs",
    query: r?.query ?? "",
    op: r?.op ?? ">",
    threshold: r ? String(r.threshold) : "",
    pendingFor: r?.pendingFor ?? "5m",
    severity: r?.severity ?? "warn",
    channels: (r?.channels ?? []).join("\n"),
    labels: Object.entries(r?.labels ?? {})
      .map(([k, v]) => `${k}=${v}`)
      .join("\n"),
    summary: r?.annotations.summary ?? "",
    runbookUrl: r?.annotations.runbook_url ?? "",
    enabled: r?.enabled ?? true,
    evalInterval: r?.evalInterval ?? "1m",
  };
}

/** Draft → UpsertAlertRule payload (camelCase protojson). */
export function ruleFromDraft(d: RuleDraft, existing?: AlertRule): AlertRuleJson {
  const annotations: Record<string, string> = { ...(existing?.annotations ?? {}) };
  if (d.summary.trim()) annotations.summary = d.summary.trim();
  else delete annotations.summary;
  if (d.runbookUrl.trim()) annotations.runbook_url = d.runbookUrl.trim();
  else delete annotations.runbook_url;
  if (d.description.trim()) annotations.description = annotations.description ?? d.description.trim();
  return {
    id: d.id || undefined,
    name: d.name.trim(),
    description: d.description.trim(),
    kind: d.kind,
    query: d.query.trim(),
    op: d.op,
    threshold: Number(d.threshold),
    pendingFor: d.pendingFor || "0s",
    severity: d.severity,
    channels: splitChannels(d.channels),
    labels: parseLabels(d.labels).labels,
    annotations,
    enabled: d.enabled,
    evalInterval: d.evalInterval || "1m",
  };
}

/** Silence matchers prefilled from an alert (alertname + its identifying labels). */
export function matchersForAlert(a: Pick<Alert, "ruleName" | "labels">): Record<string, string> {
  const keep = ["cluster", "namespace", "workload", "pod", "team", "policy", "service"];
  const m: Record<string, string> = { alertname: a.ruleName };
  for (const k of keep) if (a.labels[k]) m[k] = a.labels[k];
  return m;
}

/** Does a silence cover an alert? (every matcher equals; alertname = rule name) */
export function silenceMatches(s: Pick<Silence, "matchers">, a: Pick<Alert, "ruleName" | "labels">): boolean {
  return Object.entries(s.matchers).every(([k, v]) => (k === "alertname" ? a.ruleName === v : a.labels[k] === v));
}

export const KIND_HELP: Record<RuleKind, { title: string; grammar: string; examples: { q: string; op: Op; threshold: number; note: string }[] }> = {
  logs: {
    title: "LogQL metric query — one alert per series",
    grammar: "sum by (<labels>) (count_over_time|rate(<log query> [<range>]))",
    examples: [
      { q: 'sum by (namespace, workload) (count_over_time({level=~"error|fatal"}[5m]))', op: ">", threshold: 100, note: "error spike" },
      { q: 'sum by (workload) (rate({namespace="shop"} |= "timeout" [5m]))', op: ">", threshold: 0.5, note: "timeouts/s" },
    ],
  },
  cost: {
    title: "Spend rate in $/hour over the last 15m",
    grammar: 'cost{<label>="<value>", …} [by (<labels>)]',
    examples: [
      { q: 'cost{namespace="ml-inference"}', op: ">", threshold: 40, note: "$/h for one namespace" },
      { q: "cost by (team)", op: ">", threshold: 120, note: "$/h per team" },
    ],
  },
  budget: {
    title: "BudgetPolicy name → burn-rate multiple (1.0 = exactly on budget)",
    grammar: "<budget-policy-name>",
    examples: [{ q: "prod-monthly-ceiling", op: ">", threshold: 1.5, note: "burning 1.5× budget" }],
  },
  anomaly: {
    title: "Spend/capacity anomalies — value is $/mo impact, one alert per anomaly",
    grammar: 'anomaly{kind="spend"|"capacity"|"posture"}',
    examples: [{ q: 'anomaly{kind="spend"}', op: ">", threshold: 1000, note: "anomalies worth > $1k/mo" }],
  },
  network: {
    title: "Network spend rate in $/hour (egress + cross-zone)",
    grammar: 'network{<label>="<value>"} [by (<labels>)]',
    examples: [{ q: 'network{namespace="shop"} by (workload)', op: ">", threshold: 5, note: "egress > $5/h" }],
  },
  event: {
    title: "Cluster-event count in a window",
    grammar: 'events{kind="<kind>", <label>="<value>"}[<range>]',
    examples: [
      { q: 'events{kind="oom_killed"}[10m]', op: ">", threshold: 0, note: "any OOM kill" },
      { q: 'events{kind="crash_loop", namespace="shop"}[15m]', op: ">=", threshold: 3, note: "crash loops" },
    ],
  },
};
