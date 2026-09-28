// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { toAllocationView, toCostSeriesView, toEfficiencyView, toRightsizing, toRightsizingView } from "./map";

describe("toAllocationView", () => {
  it("maps protojson with omitted zeros and int64 strings", () => {
    const v = toAllocationView(
      {
        allocations: [
          { name: "shop", properties: { cluster: "eks-use1-prod", namespace: "shop" }, cpuCost: 40.5, ramCost: 10, totalCost: 50.5, cpuEfficiency: 0.3 },
          { name: "__idle__", idleCost: 12 },
          { name: "web" }, // everything omitted
        ],
        startUnixMs: "1790000000000",
        endUnixMs: "1790604800000",
        source: "live",
      },
      { startMs: 1, endMs: 2 },
    );
    expect(v.startMs).toBe(1_790_000_000_000);
    expect(v.endMs).toBe(1_790_604_800_000);
    expect(v.rows[0]).toMatchObject({ name: "shop", cpuCost: 40.5, ramCost: 10, gpuCost: 0, totalCost: 50.5, isIdle: false });
    expect(v.rows[1]).toMatchObject({ isIdle: true, totalCost: 12 }); // total derived from parts
    expect(v.rows[2]).toMatchObject({ name: "web", totalCost: 0, cpuEfficiency: 0 });
    expect(v.totals.totalCost).toBeCloseTo(62.5);
  });

  it("prefers the server's totals row", () => {
    const v = toAllocationView({ allocations: [{ name: "a", totalCost: 1 }], totals: { totalCost: 99 } }, { startMs: 5, endMs: 6 });
    expect(v.totals.totalCost).toBe(99);
    expect(v.startMs).toBe(5);
  });
});

describe("toCostSeriesView", () => {
  it("aligns series over the union of timestamps and puts other last", () => {
    const v = toCostSeriesView(
      {
        series: [
          { labels: { namespace: "other" }, points: [{ tsUnixMs: "1000", value: 1 }] },
          { labels: { namespace: "shop" }, points: [{ tsUnixMs: "1000", value: 5 }, { tsUnixMs: "2000", value: 7 }] },
          { labels: { namespace: "web" }, points: [{ tsUnixMs: "2000", value: 2 }] },
        ],
        forecastMonthUsd: 1234,
      },
      "namespace",
    );
    expect(v.times).toEqual([1000, 2000]);
    expect(v.stepMs).toBe(1000);
    expect(v.series.map((s) => s.key)).toEqual(["shop", "web", "other"]);
    expect(v.series[1].values).toEqual([0, 2]);
    expect(v.totalUsd).toBe(15); // derived when omitted
    expect(v.forecastMonthUsd).toBe(1234);
  });
});

describe("toRightsizing", () => {
  it("reads uint64 byte fields sent as strings and defaults enums", () => {
    const r = toRightsizing({
      cluster: "c",
      namespace: "n",
      workload: "w",
      container: "api",
      cpuRequestCores: 16,
      cpuRecommendedCores: 0.5,
      memRequestBytes: "68719476736",
      memRecommendedBytes: "7516192768",
      savingsUsdMonth: 8600,
      samples: "8064",
      confidence: "extreme",
      direction: "downsize",
    });
    expect(r.id).toBe("c/n/w/api");
    expect(r.mem.request).toBe(64 * 1024 ** 3);
    expect(r.mem.recommended).toBe(7 * 1024 ** 3);
    expect(r.samples).toBe(8064);
    expect(r.confidence).toBe("low");
    expect(r.direction).toBe("downsize");
    expect(r.replicas).toBe(1);
    expect(r.oomKills).toBe(0);
  });

  it("sums only positive savings when the total is omitted", () => {
    const v = toRightsizingView({ recommendations: [{ savingsUsdMonth: 100 }, { savingsUsdMonth: -40 }] });
    expect(v.totalSavingsUsdMonth).toBe(100);
  });
});

describe("toEfficiencyView", () => {
  it("clamps the score", () => {
    expect(toEfficiencyView({ score: 140 }).score).toBe(100);
    expect(toEfficiencyView({}).clusters).toEqual([]);
  });
});
