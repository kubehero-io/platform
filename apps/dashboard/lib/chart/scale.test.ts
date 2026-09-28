// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  formatAgo,
  formatBytes,
  formatCompact,
  formatCores,
  formatDurationNs,
  formatTimeTick,
  formatUsd,
  linear,
  niceStep,
  niceTicks,
  timeTicks,
} from "./scale";
import { normalizeLevel, seriesColor, OTHER_COLOR, SERIES } from "./palette";

describe("scales", () => {
  it("maps linearly, including inverted ranges", () => {
    const y = linear([0, 100], [200, 0]);
    expect(y(0)).toBe(200);
    expect(y(50)).toBe(100);
    expect(y(100)).toBe(0);
  });

  it.each([
    [100, 4, 25],
    [1, 5, 0.2],
    [7300, 4, 2000],
    [0.9, 3, 0.5],
  ])("niceStep(%d, %d) = %d", (span, count, want) => {
    expect(niceStep(span, count)).toBeCloseTo(want);
  });

  it("pads the top to a nice max and starts at a clean min", () => {
    expect(niceTicks(0, 87, 4)).toEqual({ ticks: [0, 25, 50, 75, 100], min: 0, max: 100 });
    const t = niceTicks(0, 0);
    expect(t.max).toBeGreaterThan(0);
  });

  it("aligns time ticks to wall-clock boundaries", () => {
    const start = Date.UTC(2026, 8, 28, 13, 7);
    const end = start + 60 * 60_000;
    const { ticks, step } = timeTicks(start, end, 4);
    expect(step).toBe(15 * 60_000);
    expect(ticks.map((t) => formatTimeTick(t, step))).toEqual(["13:15", "13:30", "13:45", "14:00"]);
  });

  it("switches to dates for multi-day spans", () => {
    const start = Date.UTC(2026, 8, 1);
    const { ticks, step } = timeTicks(start, start + 30 * 86_400_000, 6);
    expect(step).toBeGreaterThanOrEqual(86_400_000);
    expect(formatTimeTick(ticks[0], step)).toMatch(/^Sep \d+$/);
  });
});

describe("formatters", () => {
  it.each([
    [0, "0"],
    [42, "42"],
    [1284, "1,284"],
    [12_900, "12.9k"],
    [4_200_000, "4.2M"],
    [-15_000, "−15k"],
    [0.042, "0.042"],
  ])("formatCompact(%d) = %s", (v, want) => {
    expect(formatCompact(v)).toBe(want);
  });

  it.each([
    [18_200, "$18.2k"],
    [4820, "$4,820"],
    [640.4, "$640"],
    [0.42, "$0.42"],
    [2_500_000, "$2.5M"],
  ])("formatUsd(%d) = %s", (v, want) => {
    expect(formatUsd(v, { cents: true })).toBe(want);
  });

  it("formats bytes, cores, durations", () => {
    expect(formatBytes(512)).toBe("512 B");
    expect(formatBytes(6 * 1024 ** 3)).toBe("6.0 GiB");
    expect(formatCores(0.41)).toBe("410m");
    expect(formatCores(16)).toBe("16");
    expect(formatCores(2.5)).toBe("2.5");
    expect(formatDurationNs(1_500_000_000)).toBe("1.5 s");
    expect(formatDurationNs(340_000_000)).toBe("340 ms");
    expect(formatAgo(4 * 3_600_000 + 12 * 60_000)).toBe("4h 12m");
  });
});

describe("palette", () => {
  it("assigns slots in order and folds overflow into the neutral", () => {
    expect(seriesColor(0)).toBe(SERIES[0]);
    expect(seriesColor(7)).toBe(SERIES[7]);
    expect(seriesColor(8)).toBe(OTHER_COLOR);
    expect(seriesColor(2, "other")).toBe(OTHER_COLOR);
  });

  it("normalizes log levels", () => {
    expect(normalizeLevel("WARNING")).toBe("warn");
    expect(normalizeLevel("err")).toBe("error");
    expect(normalizeLevel("panic")).toBe("fatal");
    expect(normalizeLevel("")).toBe("unknown");
    expect(normalizeLevel("debug")).toBe("debug");
  });
});
