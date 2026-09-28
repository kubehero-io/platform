// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  draftFromRule,
  matchersForAlert,
  parseDurationMs,
  parseLabels,
  ruleFromDraft,
  safeLinkPath,
  silenceMatches,
  toAlert,
  toRule,
  toTestResult,
  validateChannel,
  validateDraft,
} from "./rules";
import { parseSignalSelector, validateRuleQuery } from "./selector";
import { demoAlerts, demoRules, demoSilences, demoTestRule } from "@/lib/demo/alerts";

describe("wire mapping", () => {
  it("defaults omitted fields and enums", () => {
    const r = toRule({ id: "r1", name: "x", kind: "bogus", op: "~", severity: "loud" });
    expect(r).toMatchObject({ kind: "logs", op: ">", severity: "warn", enabled: false, pendingFor: "0s", evalInterval: "1m", threshold: 0, channels: [] });
    const a = toAlert({ state: "weird", value: 3, linkPath: "//evil.example/x" });
    expect(a).toMatchObject({ state: "firing", ruleName: "(unnamed rule)", value: 3, linkPath: "" });
    expect(toTestResult({ results: [{ labels: { a: "b" }, value: 2, triggered: true }, {}] }).results).toEqual([
      { labels: { a: "b" }, value: 2, triggered: true },
      { labels: {}, value: 0, triggered: false },
    ]);
  });

  it("only keeps same-origin link paths", () => {
    expect(safeLinkPath("/logs?q=x")).toBe("/logs?q=x");
    expect(safeLinkPath("https://evil.example")).toBe("");
    expect(safeLinkPath("/\\evil")).toBe("");
  });
});

describe("validation", () => {
  it("parses durations", () => {
    expect(parseDurationMs("5m")).toBe(300_000);
    expect(parseDurationMs("1h30m")).toBe(5_400_000);
    expect(parseDurationMs("5 minutes")).toBeNull();
  });

  it.each([
    ["slack://ops-oncall", null],
    ["pagerduty://payments", null],
    ["webhook+https://hooks.example.com/kh", null],
    ["alertmanager+https://am:9093", null],
    ["discord+https://discord.com/api/webhooks/1/x", null],
    ["ftp://nope", 'unknown channel scheme "ftp://"'],
    ["ops-oncall", '"ops-oncall" is not a channel URL (e.g. slack://ops-oncall)'],
  ])("channel %s", (c, want) => {
    expect(validateChannel(c)).toBe(want);
  });

  it("parses labels and rejects bad keys", () => {
    expect(parseLabels("team=payments\nenv=prod")).toEqual({ labels: { team: "payments", env: "prod" }, error: null });
    expect(parseLabels("bad key=x").error).toMatch(/invalid label name/);
    expect(parseLabels("novalue").error).toMatch(/key=value/);
  });

  it("flags every invalid field of a draft", () => {
    const e = validateDraft({ ...draftFromRule(), name: "", threshold: "abc", pendingFor: "soon", evalInterval: "5s", channels: "ftp://x", runbookUrl: "notaurl" });
    expect(Object.keys(e).sort()).toEqual(["channels", "evalInterval", "name", "pendingFor", "query", "runbookUrl", "threshold"]);
  });

  it("round-trips a rule through the editor draft", () => {
    const rule = demoRules().find((r) => r.id === "rule-payments-timeouts")!;
    const back = ruleFromDraft(draftFromRule(rule), rule);
    expect(back).toMatchObject({
      id: rule.id,
      name: rule.name,
      kind: rule.kind,
      query: rule.query,
      op: rule.op,
      threshold: rule.threshold,
      pendingFor: rule.pendingFor,
      severity: rule.severity,
      channels: rule.channels,
      enabled: true,
    });
    expect(back.annotations?.runbook_url).toBe(rule.annotations.runbook_url);
    expect(validateDraft(draftFromRule(rule))).toEqual({});
  });
});

describe("signal selectors", () => {
  it("parses matchers, by() and ranges", () => {
    expect(parseSignalSelector('events{kind="oom_killed", namespace="prod"}[10m]', "events")).toEqual({
      ok: true,
      sel: { metric: "events", matchers: { kind: "oom_killed", namespace: "prod" }, by: [], rangeMs: 600_000 },
    });
    expect(parseSignalSelector("cost by (team)", "cost")).toEqual({ ok: true, sel: { metric: "cost", matchers: {}, by: ["team"], rangeMs: null } });
    expect(parseSignalSelector('cost{a=b}', "cost").ok).toBe(false);
    expect(parseSignalSelector('network{a="b"}', "cost")).toEqual({ ok: false, error: 'this kind queries "cost{…}", not "network"' });
  });

  it.each([
    ["logs", '{level="error"}', /metric query/],
    ["logs", 'sum by (ns) (count_over_time({level="error"}[5m]))', null],
    ["event", 'events{namespace="x"}[5m]', /need a kind/],
    ["event", 'events{kind="oom_killed"}', /need a window/],
    ["cost", 'cost{namespace="x"}[5m]', /no \[range\]/],
    ["budget", "prod-monthly-ceiling", null],
    ["budget", "Not A Name", /BudgetPolicy name/],
  ])("%s query %s", (kind, q, want) => {
    const r = validateRuleQuery(kind, q);
    if (want === null) expect(r).toBeNull();
    else expect(r).toMatch(want);
  });
});

describe("silences", () => {
  it("prefills matchers from an alert and matches alertname to the rule", () => {
    const a = demoAlerts(Date.UTC(2026, 8, 28, 14)).find((x) => x.id === "alert-oom-cart")!;
    const m = matchersForAlert(a);
    expect(m).toMatchObject({ alertname: "OOM kills", namespace: "shop", workload: "cart" });
    expect(silenceMatches({ matchers: m }, a)).toBe(true);
    expect(silenceMatches({ matchers: { ...m, namespace: "other" } }, a)).toBe(false);
  });

  it("marks the demo's silenced alert consistently with the active silence", () => {
    const now = Date.UTC(2026, 8, 28, 14);
    const silenced = demoAlerts(now).filter((a) => a.silenced);
    const active = demoSilences(now);
    expect(silenced.every((a) => active.some((s) => silenceMatches(s, a)))).toBe(true);
    expect(demoSilences(now, true).length).toBeGreaterThan(active.length);
  });
});

describe("demo TestAlertRule", () => {
  const now = Date.UTC(2026, 8, 28, 14, 7);
  it("evaluates every kind", () => {
    for (const r of demoRules()) {
      const t = demoTestRule(r, now);
      expect(t.error).toBe("");
      for (const x of t.results) expect(Number.isFinite(x.value)).toBe(true);
    }
  });

  it("fires the payments rule during the incident", () => {
    const rule = demoRules().find((r) => r.id === "rule-payments-timeouts")!;
    const t = demoTestRule(rule, now);
    expect(t.results[0].labels).toEqual({ workload: "payments" });
    expect(t.results[0].triggered).toBe(true);
  });

  it("reports errors instead of throwing", () => {
    expect(demoTestRule({ kind: "budget", query: "nope", op: ">", threshold: 1 }, now).error).toMatch(/no BudgetPolicy/);
    expect(demoTestRule({ kind: "logs", query: '{app="x"}', op: ">", threshold: 1 }, now).error).toMatch(/metric query/);
  });
});
