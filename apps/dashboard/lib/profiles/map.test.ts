// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { formatProfileValue, toFlamegraph, toProfileTarget, toTopFunctions } from "./map";
import { demoFlamegraph, demoProfileTargets, demoTopFunctions } from "@/lib/demo/profiles";

describe("toFlamegraph", () => {
  it("reads int64 strings, recomputes depth and repairs bad parents", () => {
    const fg = toFlamegraph({
      nodes: [
        { name: "total", parent: -1, total: "1000" },
        { name: "main", parent: 0, total: "1000", depth: 7 },
        { name: "work", parent: 1, self: "600", total: "600" },
        { name: "orphan", parent: 42, self: "400", total: "400" }, // bad index → root
      ],
      unit: "nanoseconds",
      costUsdMonth: 3600,
    });
    expect(fg.nodes.map((n) => [n.name, n.parent, n.depth])).toEqual([
      ["total", -1, 0],
      ["main", 0, 1],
      ["work", 1, 2],
      ["orphan", 0, 1],
    ]);
    expect(fg.total).toBe(1000);
    expect(fg.nodes[2].self).toBe(600);
    expect(fg.baselineTotal).toBe(0);
    expect(fg.type).toBe("cpu");
  });

  it("maps targets and top functions with defaults", () => {
    expect(toProfileTarget({ workload: "api", lastSeenUnixMs: "1790000000000" })).toMatchObject({
      service: "api",
      workload: "api",
      origin: "ebpf",
      types: [],
      lastSeenMs: 1_790_000_000_000,
    });
    expect(toTopFunctions({ functions: [{ name: "f", self: "5" }] })[0]).toEqual({
      name: "f",
      self: 5,
      total: 0,
      selfPct: 0,
      totalPct: 0,
      selfCostUsdMonth: 0,
    });
  });

  it("formats values by unit", () => {
    expect(formatProfileValue(3.6e12, "nanoseconds")).toBe("1.0 h");
    expect(formatProfileValue(2.5e9, "nanoseconds")).toBe("2.50 s");
    expect(formatProfileValue(5 * 1024 * 1024, "bytes")).toBe("5.0 MiB");
    expect(formatProfileValue(1500, "count")).toBe("1.5k");
  });
});

describe("demo profiles", () => {
  const NOW = Date.UTC(2026, 8, 28, 14);

  it("lists priced targets, most expensive first", () => {
    const t = demoProfileTargets(NOW);
    expect(t.length).toBeGreaterThan(8);
    for (let i = 1; i < t.length; i++) expect(t[i - 1].costUsdMonth).toBeGreaterThanOrEqual(t[i].costUsdMonth);
    expect(t.find((x) => x.service === "checkout")?.types).toContain("alloc_space");
  });

  it("builds a consistent flamegraph with a json.Marshal regression in diff mode", () => {
    const fg = demoFlamegraph({ service: "checkout", namespace: "shop", startMs: NOW - 3_600_000, endMs: NOW, diff: true })!;
    expect(fg.nodes[0].total).toBe(fg.total);
    // children never exceed their parent
    for (let i = 1; i < fg.nodes.length; i++) {
      const p = fg.nodes[fg.nodes[i].parent];
      expect(fg.nodes[i].total).toBeLessThanOrEqual(p.total);
    }
    const marshal = fg.nodes.find((n) => n.name === "encoding/json.Marshal")!;
    const share = marshal.total / fg.total;
    const baseShare = marshal.baselineTotal / fg.baselineTotal;
    expect(share / baseShare - 1).toBeGreaterThan(0.25);
  });

  it("prices top functions from the workload's cpu spend", () => {
    const fns = demoTopFunctions({ service: "checkout", namespace: "shop", startMs: NOW - 3_600_000, endMs: NOW });
    const sumSelfPct = fns.reduce((s, f) => s + f.selfPct, 0);
    expect(sumSelfPct).toBeLessThanOrEqual(100.0001);
    expect(fns[0].selfCostUsdMonth).toBeGreaterThan(0);
  });
});
