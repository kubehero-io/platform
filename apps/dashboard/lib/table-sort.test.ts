// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { matchesQuery, parseSort, sortRows } from "./table-sort";

type Row = { name: string; cost: number | null };
const rows: Row[] = [
  { name: "b", cost: 5 },
  { name: "a", cost: null },
  { name: "c", cost: 9 },
  { name: "d", cost: 5 },
];
const acc = { name: (r: Row) => r.name, cost: (r: Row) => r.cost };

describe("parseSort", () => {
  it("accepts allowed keys and directions", () => {
    expect(parseSort({ sort: "name", dir: "asc" }, ["name", "cost"] as const, { key: "cost", dir: "desc" })).toEqual({ key: "name", dir: "asc" });
  });
  it("falls back on junk", () => {
    expect(parseSort({ sort: "drop table", dir: "sideways" }, ["name", "cost"] as const, { key: "cost", dir: "desc" })).toEqual({ key: "cost", dir: "desc" });
  });
});

describe("sortRows", () => {
  it("sorts numbers desc, keeps ties stable and nulls last", () => {
    expect(sortRows(rows, { key: "cost", dir: "desc" }, acc).map((r) => r.name)).toEqual(["c", "b", "d", "a"]);
    expect(sortRows(rows, { key: "cost", dir: "asc" }, acc).map((r) => r.name)).toEqual(["b", "d", "c", "a"]);
  });
  it("sorts strings", () => {
    expect(sortRows(rows, { key: "name", dir: "asc" }, acc).map((r) => r.name)).toEqual(["a", "b", "c", "d"]);
  });
});

describe("matchesQuery", () => {
  it("requires every term", () => {
    expect(matchesQuery("shop pay", "shop", "payments")).toBe(true);
    expect(matchesQuery("shop cart", "shop", "payments")).toBe(false);
    expect(matchesQuery("  ", "x")).toBe(true);
  });
});
