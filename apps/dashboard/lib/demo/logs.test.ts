// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { anchorOf, countAt, demoContext, demoLogLabels, demoLogPatterns, demoLogVolume, demoQueryLogs, demoStreams, demoTail } from "./logs";
import { LogQLError, parseLogQL } from "@/lib/logql/parse";

const NOW = Date.UTC(2026, 8, 28, 14, 7, 30);
const HOUR = 3_600_000;

describe("demo log store", () => {
  it("is deterministic", () => {
    const a = demoQueryLogs('{namespace="shop"}', { startMs: NOW - HOUR, endMs: NOW, limit: 50, direction: "backward", stepMs: 60_000, now: NOW });
    const b = demoQueryLogs('{namespace="shop"}', { startMs: NOW - HOUR, endMs: NOW, limit: 50, direction: "backward", stepMs: 60_000, now: NOW });
    expect(a).toEqual(b);
  });

  it("returns newest-first lines inside the range, all matching the selector", () => {
    const r = demoQueryLogs('{namespace="shop", workload="payments"}', { startMs: NOW - HOUR, endMs: NOW, limit: 200, direction: "backward", stepMs: 60_000, now: NOW });
    if (r.resultType !== "streams") throw new Error("expected streams");
    expect(r.lines).toHaveLength(200);
    for (let i = 1; i < r.lines.length; i++) expect(r.lines[i - 1].tsMs).toBeGreaterThanOrEqual(r.lines[i].tsMs);
    expect(r.lines.every((l) => l.labels.workload === "payments" && l.tsMs < NOW && l.tsMs >= NOW - HOUR)).toBe(true);
    expect(r.lines[0].tsNs).toMatch(/^\d{19}$/);
  });

  it("applies line filters per line and json label filters", () => {
    const opts = { startMs: NOW - HOUR, endMs: NOW, limit: 100, direction: "backward" as const, stepMs: 60_000, now: NOW };
    const t = demoQueryLogs('{namespace="shop"} |= "timed out"', opts);
    if (t.resultType !== "streams") throw new Error("expected streams");
    expect(t.lines.length).toBeGreaterThan(0);
    expect(t.lines.every((l) => l.body.includes("timed out"))).toBe(true);
    const j = demoQueryLogs('{namespace="edge", workload="frontend-gateway"} | json | status >= 500', opts);
    if (j.resultType !== "streams") throw new Error("expected streams");
    expect(j.lines.length).toBeGreaterThan(0);
    expect(j.lines.every((l) => Number(l.labels.status) >= 500)).toBe(true);
  });

  it("volume counts equal the number of lines the list can return", () => {
    const start = NOW - 10 * 60_000;
    const v = demoLogVolume('{namespace="shop", workload="cart"}', { startMs: start, endMs: NOW, stepMs: 60_000, groupBy: "level", now: NOW });
    const r = demoQueryLogs('{namespace="shop", workload="cart"}', { startMs: start, endMs: NOW, limit: 5000, direction: "backward", stepMs: 60_000, now: NOW });
    if (r.resultType !== "streams") throw new Error("expected streams");
    // Lines stamped after "now" inside the current minute are excluded from lists.
    expect(Math.abs(v.totalLines - r.lines.length)).toBeLessThanOrEqual(v.series.reduce((a, s) => a + (s.values.at(-1) ?? 0), 0));
    expect(v.totalBytes).toBeGreaterThan(v.totalLines * 100);
  });

  it("starts the payments incident ~70 minutes ago and keeps it going", () => {
    const s = demoStreams().find((x) => x.template.id === "pay-stripe-timeout")!;
    const anchorMin = anchorOf(NOW) / 60_000;
    const during = [...Array(20)].reduce((a, _, i) => a + countAt(s, anchorMin - 40 + i, anchorMin), 0);
    const before = [...Array(20)].reduce((a, _, i) => a + countAt(s, anchorMin - 200 + i, anchorMin), 0);
    expect(during).toBeGreaterThan(before * 20);
  });

  it("evaluates metric queries with grouping", () => {
    const r = demoQueryLogs('sum by (workload) (count_over_time({namespace="shop"}[5m]))', {
      startMs: NOW - HOUR,
      endMs: NOW,
      limit: 100,
      direction: "backward",
      stepMs: 5 * 60_000,
      now: NOW,
    });
    if (r.resultType !== "matrix") throw new Error("expected matrix");
    expect(r.series.map((s) => s.labels.workload).sort()).toContain("payments");
    expect(r.series.every((s) => Object.keys(s.labels).join() === "workload")).toBe(true);
    const topk = demoQueryLogs('topk(2, sum by (workload) (rate({namespace="shop"}[5m])))', {
      startMs: NOW - HOUR,
      endMs: NOW,
      limit: 100,
      direction: "backward",
      stepMs: 5 * 60_000,
      now: NOW,
    });
    if (topk.resultType !== "matrix") throw new Error("expected matrix");
    expect(topk.series).toHaveLength(2);
  });

  it("clusters lines into patterns whose shares sum to 100", () => {
    const p = demoLogPatterns('{namespace="shop"}', { startMs: NOW - HOUR, endMs: NOW, limit: 50, now: NOW });
    expect(p.patterns.length).toBeGreaterThan(5);
    expect(p.patterns.reduce((a, x) => a + x.sharePct, 0)).toBeCloseTo(100, 5);
    expect(p.patterns[0].pattern).toContain("<_>");
    expect(p.patterns.every((x) => x.trend.length > 0 && x.sample.length > 0)).toBe(true);
  });

  it("lists label names and scoped values", () => {
    expect(demoLogLabels().names).toEqual(expect.arrayContaining(["namespace", "pod", "level", "workload", "cluster"]));
    expect(demoLogLabels("workload", '{namespace="shop"}').values).toEqual(["cart", "catalog", "checkout", "payments", "postgres", "redis", "storefront"]);
  });

  it("finds context lines in the same pod", () => {
    const r = demoQueryLogs('{namespace="shop", workload="payments"}', { startMs: NOW - HOUR, endMs: NOW, limit: 1, direction: "backward", stepMs: 60_000, now: NOW });
    if (r.resultType !== "streams") throw new Error("expected streams");
    const c = demoContext(r.lines[0], 5, NOW);
    expect(c.before.length).toBe(5);
    expect(c.before.every((l) => l.labels.pod === r.lines[0].labels.pod && l.tsMs <= r.lines[0].tsMs)).toBe(true);
  });

  it("tails only new lines", () => {
    const lines = demoTail('{namespace="shop"}', NOW - 2000, NOW, NOW);
    expect(lines.every((l) => l.tsMs > NOW - 2000 && l.tsMs <= NOW)).toBe(true);
  });
});

describe("demo LogQL parser", () => {
  it("rejects empty-matching selectors and bad regexes with a position", () => {
    expect(() => parseLogQL('{app=~".*"}')).toThrow(LogQLError);
    expect(() => parseLogQL('{app="x"} |~ "("')).toThrow(/invalid regex/);
    try {
      parseLogQL("{bad");
    } catch (e) {
      expect((e as LogQLError).pos).toBe(4);
    }
  });

  it("parses a full pipeline and a nested metric expression", () => {
    const q = parseLogQL('{a="b", c!~"d.*"} |= "x" != "y" | logfmt | dur > 1s | level="warn"');
    expect(q.type).toBe("log");
    if (q.type !== "log") return;
    expect(q.matchers).toHaveLength(2);
    expect(q.stages.map((s) => s.kind)).toEqual(["line", "line", "parser", "label", "label"]);
    const m = parseLogQL('sum by (ns) (rate({a="b"}[5m])) > 3');
    expect(m).toMatchObject({ type: "binop", op: ">", scalar: 3, expr: { type: "agg", fn: "sum", by: ["ns"], expr: { type: "range", fn: "rate", rangeMs: 300_000 } } });
  });
});
