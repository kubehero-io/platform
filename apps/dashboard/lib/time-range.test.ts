// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  DAY,
  HOUR,
  MINUTE,
  autoStep,
  bucketStarts,
  costWindowRange,
  monthlyFactor,
  parseInstant,
  resolveRange,
} from "./time-range";

const NOW = Date.UTC(2026, 8, 28, 14, 5, 30); // Mon Sep 28 2026

describe("resolveRange", () => {
  it("resolves a relative window", () => {
    const r = resolveRange({ window: "6h" }, { defaultWindow: "1h", now: NOW });
    expect(r).toMatchObject({ startMs: NOW - 6 * HOUR, endMs: NOW, key: "6h", custom: false, label: "last 6h" });
  });

  it("falls back to the default for unknown or disallowed windows", () => {
    expect(resolveRange({ window: "13h" }, { defaultWindow: "1h", now: NOW }).key).toBe("1h");
    expect(resolveRange({ window: "30d" }, { defaultWindow: "1h", allowed: ["1h", "6h"], now: NOW }).key).toBe("1h");
  });

  it("prefers a valid absolute range", () => {
    const from = NOW - 30 * MINUTE;
    const r = resolveRange({ window: "7d", from: String(from), to: String(NOW) }, { defaultWindow: "1h", now: NOW });
    expect(r).toMatchObject({ startMs: from, endMs: NOW, custom: true });
    expect(r.label).toBe("Sep 28 13:35 → 14:05 utc");
  });

  it.each([
    ["to before from", NOW, NOW - HOUR],
    ["over 90 days", NOW - 91 * DAY, NOW],
    ["entirely in the future", NOW + 2 * HOUR, NOW + 3 * HOUR],
  ])("rejects an absolute range that is %s", (_why, from, to) => {
    const r = resolveRange({ from: String(from), to: String(to) }, { defaultWindow: "1h", now: NOW });
    expect(r.custom).toBe(false);
  });

  it("parses seconds, millis and ISO", () => {
    expect(parseInstant("1790000000")).toBe(1_790_000_000_000);
    expect(parseInstant("1790000000000")).toBe(1_790_000_000_000);
    expect(parseInstant("2026-09-28T14:00:00Z")).toBe(Date.UTC(2026, 8, 28, 14));
    expect(parseInstant("yesterday-ish")).toBeNull();
  });
});

describe("steps and buckets", () => {
  it.each([
    [HOUR, 60, MINUTE],
    [6 * HOUR, 60, 10 * MINUTE],
    [7 * DAY, 60, 3 * HOUR],
    [15 * MINUTE, 60, 15_000],
  ])("autoStep(%d, %d) = %d", (span, buckets, want) => {
    expect(autoStep(span, buckets)).toBe(want);
  });

  it("aligns buckets to the step", () => {
    const b = bucketStarts(NOW - HOUR, NOW, 15 * MINUTE);
    expect(b[0] % (15 * MINUTE)).toBe(0);
    expect(b).toHaveLength(5);
  });
});

describe("cost windows", () => {
  it("resolves calendar windows in UTC", () => {
    const midnight = Date.UTC(2026, 8, 28);
    expect(costWindowRange("today", NOW)).toEqual({ startMs: midnight, endMs: NOW });
    expect(costWindowRange("yesterday", NOW)).toEqual({ startMs: midnight - DAY, endMs: midnight });
    expect(costWindowRange("week", NOW).startMs).toBe(midnight); // Monday
    expect(costWindowRange("month", NOW).startMs).toBe(Date.UTC(2026, 8, 1));
    expect(costWindowRange("lastmonth", NOW)).toEqual({ startMs: Date.UTC(2026, 7, 1), endMs: Date.UTC(2026, 8, 1) });
  });

  it("scales window spend to a month", () => {
    expect(monthlyFactor(0, 730 * HOUR)).toBeCloseTo(1);
    expect(monthlyFactor(0, DAY)).toBeCloseTo(730 / 24);
  });
});
