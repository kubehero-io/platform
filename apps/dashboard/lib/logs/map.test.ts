// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { alignSeries, prettyJson, seriesLabel, toLogLine, toLogLines, toLogPatterns, toLogVolume } from "./map";

describe("toLogLine", () => {
  it("keeps nanosecond precision and falls back to the level label", () => {
    const l = toLogLine({ tsUnixNano: "1790000000123456789", body: "hello", labels: { namespace: "shop", pod: "p", level: "warn" } });
    expect(l.tsNs).toBe("1790000000123456789");
    expect(l.tsMs).toBeCloseTo(1790000000123.4568, 3);
    expect(l.level).toBe("warn");
    expect(l.traceId).toBeUndefined();
  });
});

describe("toLogLines", () => {
  it("dedupes identical lines and orders by the full ns timestamp", () => {
    const dup = { tsUnixNano: "1790000000000000002", body: "b", labels: { pod: "p" } };
    const lines = toLogLines([
      { tsUnixNano: "1790000000000000001", body: "a", labels: { pod: "p" } },
      dup,
      dup,
      { tsUnixNano: "1790000000000000003", body: "c", labels: { pod: "p" } },
    ]);
    expect(lines.map((l) => l.body)).toEqual(["c", "b", "a"]);
    expect(toLogLines(undefined)).toEqual([]);
    expect(toLogLines([{ tsUnixNano: "2", body: "x" }, { tsUnixNano: "1", body: "y" }], "forward").map((l) => l.body)).toEqual(["y", "x"]);
  });
});

describe("series", () => {
  it("aligns sparse series on the union of timestamps", () => {
    const m = alignSeries([
      { labels: { workload: "a" }, points: [{ tsUnixMs: "1000", value: 1 }, { tsUnixMs: "3000", value: 3 }] },
      { labels: { workload: "b" }, points: [{ tsUnixMs: "2000", value: 2 }] },
    ]);
    expect(m.times).toEqual([1000, 2000, 3000]);
    expect(m.stepMs).toBe(1000);
    expect(m.series[0].values).toEqual([1, null, 3]);
    expect(m.series[1].values).toEqual([null, 2, null]);
  });

  it("labels series by their values", () => {
    expect(seriesLabel({ pod: "p1", namespace: "shop" })).toBe("shop · p1");
    expect(seriesLabel({})).toBe("{}");
  });

  it("maps volume with omitted zeros", () => {
    const v = toLogVolume({ series: [{ labels: { level: "error" }, points: [{ tsUnixMs: "60000" }] }], totalLines: "42" });
    expect(v.series[0]).toEqual({ key: "error", values: [0] });
    expect(v.totalLines).toBe(42);
    expect(v.totalBytes).toBe(0);
    expect(v.estCostUsdMonth).toBe(0);
  });

  it("maps patterns and sorts their trend by time", () => {
    const p = toLogPatterns({
      patterns: [{ pattern: "GET <_>", count: "900", trend: [{ tsUnixMs: "2", value: 5 }, { tsUnixMs: "1", value: 4 }] }],
      linesAnalyzed: "1000",
    });
    expect(p.patterns[0]).toMatchObject({ count: 900, trend: [4, 5], level: "", sharePct: 0 });
    expect(p.linesAnalyzed).toBe(1000);
  });
});

describe("prettyJson", () => {
  it("pretty-prints objects only", () => {
    expect(prettyJson('{"a":1}')).toBe('{\n  "a": 1\n}');
    expect(prettyJson("plain text")).toBeNull();
    expect(prettyJson("{broken")).toBeNull();
  });
});
