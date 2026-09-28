// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { allocate, allocationCsv, csvField, dimValue, drillTarget, type AllocAtom } from "./allocation";
import { sumRows } from "./map";

function atom(p: Partial<AllocAtom>): AllocAtom {
  return {
    cluster: "c1",
    namespace: "ns",
    workload: "w",
    controllerKind: "Deployment",
    team: "t",
    costCenter: "cc",
    node: "n1",
    nodepool: "pool",
    zone: "z1",
    labels: {},
    cpuCost: 0,
    ramCost: 0,
    gpuCost: 0,
    networkCost: 0,
    pvCost: 0,
    recoverableCost: 0,
    cpuReqCores: 0,
    cpuUseCores: 0,
    ramReqBytes: 0,
    ramUseBytes: 0,
    gpuHours: 0,
    logIngestGb: 0,
    ...p,
  };
}

// c1: shop 60 + web 30 + kube-system 10 ; c2: batch 50. Idle: c1 20, c2 5.
const ATOMS = [
  atom({ namespace: "shop", workload: "api", cpuCost: 40, ramCost: 20, cpuReqCores: 4, cpuUseCores: 1, ramReqBytes: 8, ramUseBytes: 6, labels: { app: "api" } }),
  atom({ namespace: "web", workload: "front", cpuCost: 20, ramCost: 10, cpuReqCores: 2, cpuUseCores: 2, ramReqBytes: 4, ramUseBytes: 1 }),
  atom({ namespace: "kube-system", workload: "coredns", cpuCost: 6, ramCost: 4 }),
  atom({ cluster: "c2", namespace: "batch", workload: "etl", cpuCost: 30, ramCost: 20, team: "data" }),
];
const IDLE = { c1: 20, c2: 5 };
const total = (rows: { totalCost: number }[]) => rows.reduce((s, r) => s + r.totalCost, 0);

describe("allocate", () => {
  it("aggregates by namespace with cost-weighted efficiency", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"] });
    expect(rows.map((r) => r.name)).toEqual(["shop", "batch", "web", "kube-system"]);
    const shop = rows[0];
    expect(shop.totalCost).toBe(60);
    expect(shop.cpuEfficiency).toBeCloseTo(0.25);
    expect(shop.ramEfficiency).toBeCloseTo(0.75);
    // (0.25×40 + 0.75×20) / 60
    expect(shop.totalEfficiency).toBeCloseTo(25 / 60);
    expect(total(rows)).toBe(150); // idle not included by default
  });

  it("keeps idle as its own row when asked, conserving the total", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"], includeIdle: true });
    const idle = rows.find((r) => r.isIdle)!;
    expect(idle.name).toBe("__idle__");
    expect(idle.idleCost).toBe(25);
    expect(total(rows)).toBe(175);
  });

  it("names idle per cluster when aggregating by cluster", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["cluster"], includeIdle: true });
    expect(rows.filter((r) => r.isIdle).map((r) => r.name).sort()).toEqual(["c1/__idle__", "c2/__idle__"]);
  });

  it("shares idle weighted by cost, within each cluster", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"], shareIdle: "weighted" });
    expect(rows.some((r) => r.isIdle)).toBe(false);
    expect(total(rows)).toBeCloseTo(175);
    const shop = rows.find((r) => r.name === "shop")!;
    expect(shop.idleCost).toBeCloseTo((20 * 60) / 100); // shop is 60 of c1's 100
    expect(rows.find((r) => r.name === "batch")!.idleCost).toBeCloseTo(5);
  });

  it("shares idle evenly per row", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"], shareIdle: "even" });
    expect(rows.find((r) => r.name === "web")!.idleCost).toBeCloseTo(20 / 3);
    expect(total(rows)).toBeCloseTo(175);
  });

  it("redistributes shared namespaces within the cluster by cost", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"], sharedNamespaces: ["kube-system"] });
    expect(rows.find((r) => r.name === "kube-system")).toBeUndefined();
    expect(rows.find((r) => r.name === "shop")!.sharedCost).toBeCloseTo((10 * 60) / 90);
    expect(rows.find((r) => r.name === "web")!.sharedCost).toBeCloseTo((10 * 30) / 90);
    expect(rows.find((r) => r.name === "batch")!.sharedCost).toBe(0); // other cluster
    expect(total(rows)).toBeCloseTo(150);
  });

  it("applies filters after sharing so a filtered view keeps its share", () => {
    const rows = allocate(ATOMS, IDLE, {
      aggregate: ["workload"],
      filters: { namespace: "shop" },
      shareIdle: "weighted",
      sharedNamespaces: ["kube-system"],
    });
    expect(rows).toHaveLength(1);
    // shared 10 × 60/90, then idle 20 × (60 + 6.67) / 100
    expect(rows[0].sharedCost).toBeCloseTo(6.667, 2);
    expect(rows[0].idleCost).toBeCloseTo((20 * (60 + 20 / 3)) / 100, 4);
  });

  it("drops idle rows under a non-cluster filter, keeps them under a cluster filter", () => {
    expect(allocate(ATOMS, IDLE, { aggregate: ["namespace"], includeIdle: true, filters: { namespace: "web" } }).some((r) => r.isIdle)).toBe(false);
    const c2 = allocate(ATOMS, IDLE, { aggregate: ["namespace"], includeIdle: true, filters: { cluster: "c2" } });
    expect(c2.find((r) => r.isIdle)!.idleCost).toBe(5);
  });

  it("aggregates by label with an unallocated bucket", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["label:app"] });
    expect(rows.map((r) => r.name).sort()).toEqual(["__unallocated__", "api"]);
    expect(dimValue(ATOMS[0], "label:missing")).toBe("__unallocated__");
  });

  it("keeps only the properties a group agrees on", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["team"] });
    const t = rows.find((r) => r.name === "t")!;
    expect(t.properties.cluster).toBe("c1");
    expect(t.properties.namespace).toBeUndefined(); // shop/web/kube-system disagree
  });

  it("totals are consistent with sumRows", () => {
    const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"], includeIdle: true });
    const t = sumRows(rows);
    expect(t.totalCost).toBeCloseTo(175);
    expect(t.idleCost).toBe(25);
  });
});

describe("drillTarget", () => {
  const rows = allocate(ATOMS, IDLE, { aggregate: ["namespace"] });
  it("namespace → workloads in that namespace", () => {
    expect(drillTarget("namespace", rows[0], {})).toEqual({ kind: "aggregate", aggregate: "workload", filters: { namespace: "shop" } });
  });
  it("workload → the workload hub", () => {
    const w = allocate(ATOMS, IDLE, { aggregate: ["workload"], filters: { namespace: "shop" } });
    expect(drillTarget("workload", w[0], { namespace: "shop" })).toEqual({ kind: "workload", cluster: "c1", namespace: "shop", workload: "api" });
  });
  it("idle rows don't drill", () => {
    const withIdle = allocate(ATOMS, IDLE, { aggregate: ["namespace"], includeIdle: true });
    expect(drillTarget("namespace", withIdle.find((r) => r.isIdle)!, {})).toBeNull();
  });
});

describe("CSV", () => {
  it("escapes and neutralises formulas", () => {
    expect(csvField('a,"b"')).toBe('"a,""b"""');
    expect(csvField("=HYPERLINK(1)")).toBe("'=HYPERLINK(1)");
    expect(csvField(1.234567)).toBe("1.2346");
    expect(csvField(Number.NaN)).toBe("");
  });
  it("writes a header and one CRLF line per row", () => {
    const csv = allocationCsv(allocate(ATOMS, IDLE, { aggregate: ["namespace"] }));
    const lines = csv.trimEnd().split("\r\n");
    expect(lines[0].startsWith("name,cluster,namespace,cpu_cost")).toBe(true);
    expect(lines).toHaveLength(5);
  });
});
