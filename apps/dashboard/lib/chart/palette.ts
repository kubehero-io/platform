// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Series colours. Categorical slots are the dataviz reference palette's
// dark steps, validated against the dashboard's raised surface (#111111):
//
//   node validate_palette.js "<SERIES>" --mode dark --surface "#111111"
//   → lightness band, chroma floor, CVD (worst adjacent ΔE 8.4),
//     normal-vision floor (19.3) and 3:1 contrast all PASS.
//
// Rules the charts follow:
//   · slots are assigned in this fixed order, never cycled; a 9th series
//     folds into "other" (OTHER_COLOR, a neutral);
//   · the design tokens' accent / signal / warn stay reserved for state
//     (urgency, healthy, warning) and are never a series colour;
//   · log levels are a severity scale, not identity, so they get their
//     own fixed mapping (LEVEL_COLOR) with the level name always shown.

export const SERIES = [
  "#3987e5", // blue
  "#d95926", // orange
  "#199e70", // aqua
  "#c98500", // yellow
  "#d55181", // magenta
  "#008300", // green
  "#9085e9", // violet
  "#e66767", // red
] as const;

export const OTHER_COLOR = "#5a5a55";

/** Colour for the i-th series (0-based); "other" / overflow → neutral. */
export function seriesColor(i: number, name?: string): string {
  if (name === "other" || name === "__other__") return OTHER_COLOR;
  return i >= 0 && i < SERIES.length ? SERIES[i] : OTHER_COLOR;
}

export const LEVEL_ORDER = ["fatal", "error", "warn", "info", "debug", "trace", "unknown"] as const;
export type LogLevel = (typeof LEVEL_ORDER)[number];

export const LEVEL_COLOR: Record<LogLevel, string> = {
  fatal: "#ff3b3b",
  error: "#e66767",
  warn: "#f5c542",
  info: "#3987e5",
  debug: "#5a5a55",
  trace: "#3a3a36",
  unknown: "#7a7a74",
};

export function normalizeLevel(raw: string | undefined): LogLevel {
  const l = (raw ?? "").toLowerCase();
  if (l === "warning") return "warn";
  if (l === "err") return "error";
  if (l === "critical" || l === "crit" || l === "panic" || l === "emergency" || l === "alert") return "fatal";
  if (l === "information" || l === "notice") return "info";
  return (LEVEL_ORDER as readonly string[]).includes(l) ? (l as LogLevel) : "unknown";
}
