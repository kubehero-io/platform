// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { capNodes, edgeClass, edgePath, edgeWidth, edgesOf, layoutServiceMap, nodeRadius, retransmitHeavy, zoneColumns } from "./layout";
import { toNetworkCosts, toServiceMap } from "./map";
import { demoNetworkCosts, demoServiceMap } from "@/lib/demo/network";
import type { MapEdge, MapNode } from "./types";

const n = (id: string, zone: string, kind = "workload", bytes = 1e9): MapNode => ({
  id,
  name: id.split("/").pop()!,
  namespace: "ns",
  kind,
  zone,
  bytesIn: bytes / 2,
  bytesOut: bytes / 2,
  costUsdMonth: 0,
});
const e = (source: string, target: string, over: Partial<MapEdge> = {}): MapEdge => ({
  id: `${source}→${target}`,
  source,
  target,
  port: 80,
  protocol: "tcp",
  bytes: 1e9,
  bytesPerSec: 1e4,
  crossZone: false,
  egress: false,
  costUsdMonth: 0,
  retransmits: 0,
  ...over,
});

const NODES = [n("w/a", "z1"), n("w/b", "z1"), n("w/c", "z2"), n("x/net", "", "external")];
const EDGES = [e("w/a", "w/b"), e("w/a", "w/c", { crossZone: true }), e("w/c", "x/net", { egress: true })];

describe("encodings", () => {
  it("scales radius and width by square root, clamped", () => {
    expect(nodeRadius(0, 100)).toBe(6);
    expect(nodeRadius(100, 100)).toBe(26);
    expect(nodeRadius(25, 100)).toBeCloseTo(6 + 20 * 0.5);
    expect(edgeWidth(1e9, 1e3)).toBe(8); // clamped
    expect(edgeWidth(0, 10)).toBe(1);
  });

  it("classifies edges; egress wins over cross-zone", () => {
    expect(edgeClass({ crossZone: true, egress: true })).toBe("egress");
    expect(edgeClass({ crossZone: true, egress: false })).toBe("cross-zone");
    expect(edgeClass({ crossZone: false, egress: false })).toBe("internal");
  });

  it("flags retransmit-heavy links above 0.5% of packets", () => {
    expect(retransmitHeavy({ bytes: 1460 * 1000, retransmits: 4 })).toBe(false);
    expect(retransmitHeavy({ bytes: 1460 * 1000, retransmits: 6 })).toBe(true);
  });
});

describe("layoutServiceMap", () => {
  const a = layoutServiceMap(NODES, EDGES, { width: 1200, height: 600 });
  const b = layoutServiceMap([...NODES].reverse(), EDGES, { width: 1200, height: 600 });

  it("is deterministic regardless of input order", () => {
    expect(a).toEqual(b);
  });

  it("keeps every node inside the canvas with no NaNs", () => {
    for (const p of a) {
      expect(Number.isFinite(p.x) && Number.isFinite(p.y)).toBe(true);
      expect(p.x - p.r).toBeGreaterThanOrEqual(0);
      expect(p.x + p.r).toBeLessThanOrEqual(1200);
      expect(p.y - p.r).toBeGreaterThanOrEqual(0);
      expect(p.y + p.r).toBeLessThanOrEqual(600);
    }
  });

  it("groups zones into columns left→right and puts externals on the right", () => {
    const x = Object.fromEntries(a.map((p) => [p.id, p.x]));
    const cols = zoneColumns(NODES, 1200);
    expect(cols.get("z1")!).toBeLessThan(cols.get("z2")!);
    expect(x["w/c"]).toBeGreaterThan(Math.min(x["w/a"], x["w/b"]));
    expect(x["x/net"]).toBeGreaterThan(x["w/c"]);
  });
});

describe("helpers", () => {
  it("draws edges from rim to rim, bending bidirectional pairs apart", () => {
    const s = { x: 0, y: 0, r: 10 };
    const t = { x: 100, y: 0, r: 10 };
    const straight = edgePath(s, t, 0);
    expect(straight.d).toBe("M10.0,0.0 Q50.0,0.0 86.0,0.0");
    const bent = edgePath(s, t, 1);
    expect(bent.my).not.toBe(0);
    expect(edgePath(t, s, 1).my).toBe(-bent.my);
  });

  it("splits a node's edges by direction", () => {
    const r = edgesOf("w/a", EDGES);
    expect(r.outbound.map((x) => x.target).sort()).toEqual(["w/b", "w/c"]);
    expect(r.inbound).toEqual([]);
  });

  it("folds the long tail into one node and merges its edges", () => {
    const many = Array.from({ length: 10 }, (_, i) => n(`w/n${i}`, "z1", "workload", (10 - i) * 1e9));
    const edges = many.slice(1).map((m) => e("w/n0", m.id));
    const c = capNodes(many, edges, 4);
    expect(c.nodes).toHaveLength(4);
    expect(c.nodes.at(-1)!.name).toBe("other (7)");
    const toOther = c.edges.filter((x) => x.target === "other:*");
    expect(toOther).toHaveLength(1);
    expect(toOther[0].bytes).toBe(7e9);
  });
});

describe("mappers + demo", () => {
  it("drops edges to unknown nodes and merges duplicates", () => {
    const m = toServiceMap({
      nodes: [{ id: "a", name: "a" }, { id: "b", name: "b" }],
      edges: [
        { source: "a", target: "b", port: 80, bytes: 1, costUsdMonth: 1 },
        { source: "a", target: "b", port: 80, bytes: 2, costUsdMonth: 2, crossZone: true },
        { source: "a", target: "zzz", port: 80, bytes: 9 },
      ],
    });
    expect(m.edges).toHaveLength(1);
    expect(m.edges[0]).toMatchObject({ bytes: 3, costUsdMonth: 3, crossZone: true });
    expect(m.totalCostUsdMonth).toBe(3);
  });

  it("derives totals when omitted", () => {
    expect(toNetworkCosts({ costs: [{ workload: "a", egressUsdMonth: 2, crossZoneUsdMonth: 1 }] })).toMatchObject({ totalUsdMonth: 3 });
  });

  it("prices the demo map consistently", () => {
    const now = Date.UTC(2026, 8, 28);
    const m = demoServiceMap({ clusterId: "eks-use1-prod", startMs: now - 86_400_000, endMs: now });
    expect(m.nodes.length).toBeGreaterThan(10);
    expect(m.edges.every((x) => !(x.egress && x.crossZone))).toBe(true);
    expect(m.totalCostUsdMonth).toBeCloseTo(m.edges.reduce((s, x) => s + x.costUsdMonth, 0));
    expect(m.edges.some(retransmitHeavy)).toBe(true);
    const costs = demoNetworkCosts({ clusterId: "eks-use1-prod", startMs: now - 86_400_000, endMs: now });
    expect(costs.costs[0].totalUsdMonth).toBeGreaterThanOrEqual(costs.costs.at(-1)!.totalUsdMonth);
    expect(costs.costs.some((c) => c.workload === "postgres" && c.crossZoneUsdMonth > 0)).toBe(true);
  });
});
