// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  ancestry,
  buildTree,
  compileSearch,
  diffOf,
  flattenFolded,
  frameCost,
  hitTest,
  packageOf,
  parseFolded,
  relativeChange,
  searchTree,
  topFunctions,
  visibleRects,
} from "./tree";

// total 100: main 100 → http 70 (json 40 [self 30 + gzip 10], db 20, self 10) , gc 30
const FOLDED = parseFolded(`
main;http;json 30 20
main;http;json;gzip 10 20
main;http;db 20 20
main;http 10 10
main;gc 30 30
`);
const nodes = flattenFolded(FOLDED);
const t = buildTree(nodes);
const idx = (name: string) => nodes.findIndex((n) => n.name === name);

describe("flattenFolded", () => {
  it("builds a pre-order tree with totals, selfs and baselines", () => {
    expect(nodes.map((n) => `${n.depth}:${n.name}:${n.total}/${n.self}`)).toEqual([
      "0:total:100/0",
      "1:main:100/0",
      "2:http:70/10",
      "3:json:40/30",
      "4:gzip:10/10",
      "3:db:20/20",
      "2:gc:30/30",
    ]);
    expect(nodes[0].baselineTotal).toBe(100);
    expect(nodes[idx("gzip")].baselineTotal).toBe(20);
    // parent indices point backwards (pre-order)
    nodes.forEach((n, i) => i > 0 && expect(n.parent).toBeLessThan(i));
  });

  it("skips malformed folded lines", () => {
    expect(parseFolded("a;b 3\nnot a stack\n\nc 1")).toHaveLength(2);
  });
});

describe("layout", () => {
  it("lays children out left-to-right in root units", () => {
    expect([t.x0[idx("http")], t.x1[idx("http")]]).toEqual([0, 0.7]);
    expect([t.x0[idx("gc")], t.x1[idx("gc")]]).toEqual([0.7, 1]);
    expect([t.x0[idx("json")], t.x1[idx("json")]]).toEqual([0, 0.4]);
    expect(t.x0[idx("db")]).toBeCloseTo(0.4);
    expect(t.x1[idx("db")]).toBeCloseTo(0.6);
    expect(t.maxDepth).toBe(4);
  });

  it("zooms by viewport: the focused node fills the width", () => {
    const json = idx("json");
    const rects = visibleRects(t, t.x0[json], t.x1[json], 400);
    const r = rects.find((x) => x.i === json)!;
    expect(r).toMatchObject({ x: 0, w: 400 });
    // gzip is 10/40 of json
    expect(rects.find((x) => x.i === idx("gzip"))!.w).toBeCloseTo(100);
    // gc is outside the viewport
    expect(rects.some((x) => x.i === idx("gc"))).toBe(false);
    // ancestors span the whole width
    expect(rects.find((x) => x.i === idx("http"))!.w).toBe(400);
  });

  it("culls sub-pixel frames", () => {
    expect(visibleRects(t, 0, 1, 1000, 150).map((r) => nodes[r.i].name)).not.toContain("gzip");
  });

  it("hit-tests by depth and x", () => {
    expect(hitTest(t, 3, 0.5)).toBe(idx("db"));
    expect(hitTest(t, 3, 0.65)).toBe(-1);
    expect(hitTest(t, 2, 0.99)).toBe(idx("gc"));
    expect(hitTest(t, 9, 0.5)).toBe(-1);
  });

  it("computes ancestry", () => {
    expect(ancestry(t, idx("gzip")).map((i) => nodes[i].name)).toEqual(["total", "main", "http", "json", "gzip"]);
  });
});

describe("search", () => {
  it("matches substrings and /regex/ and counts nested matches once", () => {
    const r = searchTree(t, compileSearch("json")!);
    expect([...r.matched].map((i) => nodes[i].name)).toEqual(["json"]);
    expect(r.fraction).toBeCloseTo(0.4);
    // "http" and "json" nest: 0.7, not 1.1
    expect(searchTree(t, compileSearch("/^(http|json)$/")!).fraction).toBeCloseTo(0.7);
    expect(compileSearch("   ")).toBeNull();
    expect(compileSearch("/(unclosed/")).toBeNull();
  });
});

describe("diff + cost", () => {
  it("compares shares normalised to each root", () => {
    const json = nodes[idx("json")];
    // 40/100 now vs 40/100 baseline → neutral
    expect(diffOf(json, 100, 100).tone).toBe("neutral");
    const gzip = nodes[idx("gzip")];
    // 10/100 vs 20/100 → improvement of 10pp, saturated at 5pp
    expect(diffOf(gzip, 100, 100)).toMatchObject({ tone: "improvement", intensity: 1 });
    expect(relativeChange(gzip, 100, 100)).toBeCloseTo(-0.5);
    expect(relativeChange({ ...gzip, baselineTotal: 0 }, 100, 100)).toBe(Infinity);
  });

  it("prices frames by their share of the root", () => {
    expect(frameCost(40, 100, 3600)).toBeCloseTo(1440);
    expect(frameCost(40, 0, 3600)).toBe(0);
  });
});

describe("topFunctions", () => {
  it("sums self and counts recursive totals once per path", () => {
    const rec = flattenFolded(parseFolded("f;g;f;h 10\nf 5"));
    const fns = Object.fromEntries(topFunctions(rec).map((f) => [f.name, f]));
    expect(fns.f.total).toBe(15); // not 25
    expect(fns.f.self).toBe(5);
    expect(fns.h.selfPct).toBeCloseTo((10 / 15) * 100);
  });
});

describe("packageOf", () => {
  it.each([
    ["encoding/json.Marshal", "encoding/json"],
    ["net/http.(*conn).serve", "net/http"],
    ["github.com/go-chi/chi/v5.(*Mux).ServeHTTP", "github.com/go-chi/chi/v5"],
    ["tokio::runtime::park", "tokio"],
    ["handler (app.js:12)", "handler"],
  ])("%s → %s", (name, pkg) => {
    expect(packageOf(name)).toBe(pkg);
  });
});
