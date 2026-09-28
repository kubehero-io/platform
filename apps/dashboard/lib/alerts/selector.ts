// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The small selector grammar the non-LogQL alert kinds share
// (alerts.proto):
//
//   cost{namespace="ml-inference"} by (team)
//   network{namespace="edge"} by (workload)
//   anomaly{kind="spend"}
//   events{kind="oom_killed", namespace="prod"}[10m]
//
// Used for the rule editor's inline validation and by the demo
// evaluator. The control plane has its own parser; this one mirrors it.

export type SignalSelector = {
  metric: string;
  matchers: Record<string, string>;
  by: string[];
  rangeMs: number | null;
};

const LABEL = "[a-zA-Z_][a-zA-Z0-9_]*";

export function parseSignalSelector(q: string, expectMetric?: string): { ok: true; sel: SignalSelector } | { ok: false; error: string } {
  const s = q.trim();
  const m = new RegExp(`^(${LABEL})\\s*(\\{([^}]*)\\})?\\s*(\\[\\s*(\\d+(?:ms|s|m|h|d))\\s*\\])?\\s*(by\\s*\\(([^)]*)\\))?\\s*$`).exec(s);
  if (!m) return { ok: false, error: `expected ${expectMetric ?? "name"}{label="value", …} [by (label, …)]` };
  const metric = m[1];
  if (expectMetric && metric !== expectMetric) return { ok: false, error: `this kind queries "${expectMetric}{…}", not "${metric}"` };
  const matchers: Record<string, string> = {};
  const body = (m[3] ?? "").trim();
  if (body) {
    for (const part of splitTopLevel(body)) {
      const mm = new RegExp(`^\\s*(${LABEL})\\s*=\\s*"((?:[^"\\\\]|\\\\.)*)"\\s*$`).exec(part);
      if (!mm) return { ok: false, error: `bad matcher "${part.trim()}" — use label="value"` };
      matchers[mm[1]] = mm[2].replace(/\\(.)/g, "$1");
    }
  }
  const by = (m[7] ?? "")
    .split(",")
    .map((x) => x.trim())
    .filter(Boolean);
  for (const b of by) if (!new RegExp(`^${LABEL}$`).test(b)) return { ok: false, error: `bad label "${b}" in by (…)` };
  let rangeMs: number | null = null;
  if (m[5]) {
    const d = /^(\d+)(ms|s|m|h|d)$/.exec(m[5])!;
    rangeMs = Number(d[1]) * { ms: 1, s: 1e3, m: 6e4, h: 3.6e6, d: 8.64e7 }[d[2] as "ms"];
  }
  return { ok: true, sel: { metric, matchers, by, rangeMs } };
}

function splitTopLevel(body: string): string[] {
  const out: string[] = [];
  let cur = "";
  let inStr = false;
  for (let i = 0; i < body.length; i++) {
    const c = body[i];
    if (c === "\\" && inStr) {
      cur += c + (body[i + 1] ?? "");
      i++;
      continue;
    }
    if (c === '"') inStr = !inStr;
    if (c === "," && !inStr) {
      out.push(cur);
      cur = "";
      continue;
    }
    cur += c;
  }
  if (cur.trim()) out.push(cur);
  return out;
}

/** Validate a rule query for its kind (fast feedback in the editor). */
export function validateRuleQuery(kind: string, query: string): string | null {
  const q = query.trim();
  if (!q) return "required";
  switch (kind) {
    case "logs":
      return /^\s*[a-z_]+\s*(by|without)?\s*(\([^)]*\))?\s*\(/.test(q) || /^\s*(count_over_time|rate|bytes_over_time|bytes_rate)\s*\(/.test(q)
        ? null
        : "logs rules need a LogQL metric query, e.g. sum by (namespace) (count_over_time({level=\"error\"}[5m]))";
    case "cost":
    case "network":
    case "anomaly": {
      const r = parseSignalSelector(q, kind);
      if (!r.ok) return r.error;
      if (r.sel.rangeMs !== null) return `${kind} queries take no [range]`;
      return null;
    }
    case "event": {
      const r = parseSignalSelector(q, "events");
      if (!r.ok) return r.error;
      if (!r.sel.matchers.kind) return 'events need a kind, e.g. events{kind="oom_killed"}[10m]';
      if (r.sel.rangeMs === null) return "events need a window, e.g. [10m]";
      return null;
    }
    case "budget":
      return /^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/.test(q) ? null : "a BudgetPolicy name, e.g. prod-monthly-ceiling";
    default:
      return "unknown kind";
  }
}
