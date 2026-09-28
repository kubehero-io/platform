// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { demoAllocation, demoCostSeries, demoEfficiency, demoIdleMonthly, demoRightsizing, efficiencyScore, recommend } from "./cost";
import { clusterMonthlyUsd, demoWorkloads, findWorkload, totalUsd } from "./world";

const NOW = Date.UTC(2026, 8, 28, 14, 5);

describe("demo world", () => {
  it("is deterministic and fills each cluster to its node spend", () => {
    const a = demoWorkloads();
    expect(demoWorkloads()).toBe(a); // memoised
    for (const cluster of ["eks-use1-prod", "gke-usc1-prod", "aks-ne-staging"]) {
      const alloc = a.filter((w) => w.cluster === cluster).reduce((s, w) => s + totalUsd(w), 0);
      const idle = demoIdleMonthly()[cluster];
      expect(alloc + idle).toBeCloseTo(Math.max(clusterMonthlyUsd(cluster), alloc + idle), 0);
      expect(idle).toBeGreaterThan(0);
    }
  });

  it("has unique workload identities", () => {
    const keys = demoWorkloads().map((w) => `${w.cluster}/${w.namespace}/${w.name}`);
    expect(new Set(keys).size).toBe(keys.length);
  });
});

describe("demoAllocation", () => {
  it("scales the monthly rate to the window and honours idle modes", () => {
    const d30 = demoAllocation({ window: "30d", aggregate: "cluster", includeIdle: true }, NOW);
    const d7 = demoAllocation({ window: "7d", aggregate: "cluster", includeIdle: true }, NOW);
    expect(d30.totals.totalCost / d7.totals.totalCost).toBeCloseTo(30 / 7, 5);
    const weighted = demoAllocation({ window: "7d", aggregate: "cluster", shareIdle: "weighted" }, NOW);
    expect(weighted.totals.totalCost).toBeCloseTo(d7.totals.totalCost, 4); // idle conserved
    expect(weighted.rows.some((r) => r.isIdle)).toBe(false);
  });

  it("drills namespace → workloads with the filter applied", () => {
    const v = demoAllocation({ window: "7d", aggregate: "workload", filters: { namespace: "shop" } }, NOW);
    expect(v.rows.map((r) => r.name)).toContain("checkout");
    expect(v.rows.every((r) => r.properties.namespace === "shop")).toBe(true);
  });
});

describe("demoCostSeries", () => {
  it("produces daily steps for 30d with top-N + other", () => {
    const v = demoCostSeries({ window: "30d", groupBy: "namespace", top: 5 }, NOW);
    expect(v.stepMs).toBe(86_400_000);
    expect(v.times.length).toBeGreaterThanOrEqual(30);
    expect(v.series).toHaveLength(6);
    expect(v.series[5].key).toBe("other");
    expect(v.series.every((s) => s.values.length === v.times.length)).toBe(true);
    expect(v.forecastMonthUsd).toBeGreaterThan(0);
  });

  it("uses hourly steps for short windows and respects filters", () => {
    const v = demoCostSeries({ window: "24h", groupBy: "workload", filters: { namespace: "shop" } }, NOW);
    expect(v.stepMs).toBe(3_600_000);
    expect(v.series.every((s) => s.key.startsWith("shop/"))).toBe(true);
  });
});

describe("demo rightsizing engine", () => {
  it("downsizes the 16-core vectordb-ingress to p95 + 15%", () => {
    const w = findWorkload("eks-use1-prod", "retrieval", "vectordb-ingress")!;
    const r = recommend(w, w.containers[0]);
    expect(r.direction).toBe("downsize");
    expect(r.cpu.recommended).toBeCloseTo(0.41 * 1.15, 2);
    expect(r.savingsUsdMonth).toBeGreaterThan(7000);
    expect(r.confidence).toBe("high");
  });

  it("never shrinks memory for an OOM-killing workload", () => {
    const w = findWorkload("eks-use1-prod", "shop", "cart")!;
    const r = recommend(w, w.containers[0]);
    expect(r.oomKills).toBe(7);
    expect(r.mem.recommended).toBeGreaterThanOrEqual(r.mem.limit * 1.25 - 1024 * 1024);
    expect(r.direction).toBe("upsize");
    expect(r.reason).toMatch(/OOM/);
  });

  it("memory never goes below the observed max", () => {
    for (const w of demoWorkloads().slice(0, 40)) {
      for (const c of w.containers) {
        const r = recommend(w, c, { headroomPct: 0 });
        expect(r.mem.recommended).toBeGreaterThanOrEqual(r.mem.max);
        expect(r.cpu.recommended).toBeGreaterThanOrEqual(0.01);
      }
    }
  });

  it("lists upsizes regardless of the savings floor, sorted by savings", () => {
    const v = demoRightsizing({ minSavingsUsdMonth: 1_000_000 });
    expect(v.recs.every((r) => r.direction === "upsize")).toBe(true);
    const all = demoRightsizing();
    for (let i = 1; i < all.recs.length; i++) expect(all.recs[i - 1].savingsUsdMonth).toBeGreaterThanOrEqual(all.recs[i].savingsUsdMonth);
  });
});

describe("efficiency", () => {
  it("scores between 0 and 100 and penalises idle", () => {
    expect(efficiencyScore(1, 1, 1, 1, 0)).toBe(100);
    expect(efficiencyScore(0.5, 0.5, 1, 1, 0.5)).toBe(25);
    const e = demoEfficiency({}, NOW);
    expect(e.score).toBeGreaterThan(0);
    expect(e.score).toBeLessThan(100);
    expect(e.clusters.length).toBeGreaterThan(3);
  });
});
