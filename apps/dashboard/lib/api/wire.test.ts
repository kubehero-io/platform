// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { arr, compareNs, int64, msToNs, nsKey, nsToMs, num, rec, str } from "./wire";

describe("protojson readers", () => {
  it.each([
    [42, 42],
    ["42", 42],
    ["1727500000123", 1727500000123],
    [undefined, 0],
    ["NaN", 0],
    ["Infinity", 0],
    [Number.NaN, 0],
    ["", 0],
    [null, 0],
    [{}, 0],
  ])("num(%j) = %d", (v, want) => {
    expect(num(v)).toBe(want);
  });

  it("int64 accepts the string form", () => {
    expect(int64("9007199254740991")).toBe(Number.MAX_SAFE_INTEGER);
  });

  it("defaults omitted zero values", () => {
    expect(str(undefined)).toBe("");
    expect(arr(undefined)).toEqual([]);
    expect(rec({ a: "1", b: 2 as unknown as string })).toEqual({ a: "1" });
  });
});

describe("nanosecond timestamps", () => {
  it("converts to ms at double precision and keeps exact ns identity as a string", () => {
    const s = "1727500000123456789";
    expect(nsToMs(s)).toBeCloseTo(1727500000123.4568, 3);
    // Number() cannot tell two lines 1ns apart; the string key can.
    expect(Number("1727500000123456789")).toBe(Number("1727500000123456790"));
    expect(compareNs(nsKey("1727500000123456789"), nsKey("1727500000123456790"))).toBeLessThan(0);
  });

  it("handles short and odd inputs", () => {
    expect(nsToMs("999999")).toBeCloseTo(0.999999);
    expect(nsToMs(2_000_000)).toBe(2);
    expect(nsToMs("abc")).toBe(0);
    expect(nsToMs(undefined)).toBe(0);
  });

  it("orders nanosecond strings numerically", () => {
    expect(compareNs("1727500000123456789", "1727500000123456790")).toBeLessThan(0);
    expect(compareNs("999", "1000")).toBeLessThan(0);
    expect(compareNs(nsKey("0001"), "1")).toBe(0);
  });

  it("round-trips ms → ns", () => {
    expect(msToNs(1727500000123)).toBe("1727500000123000000");
    expect(nsToMs(msToNs(1727500000123.5))).toBeCloseTo(1727500000123.5, 3);
  });
});
