// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// URL-driven time ranges. Every explorer keeps its range in the query
// string so any view is a shareable link:
//
//   ?window=1h                      relative, re-evaluated on each load
//   ?from=<ms>&to=<ms>              absolute (drag-to-zoom, custom picker)
//
// Pure; `now` is injectable for tests.

export const MINUTE = 60_000;
export const HOUR = 60 * MINUTE;
export const DAY = 24 * HOUR;

const RELATIVE: Record<string, number> = {
  "5m": 5 * MINUTE,
  "15m": 15 * MINUTE,
  "30m": 30 * MINUTE,
  "1h": HOUR,
  "3h": 3 * HOUR,
  "6h": 6 * HOUR,
  "12h": 12 * HOUR,
  "24h": DAY,
  "2d": 2 * DAY,
  "7d": 7 * DAY,
  "14d": 14 * DAY,
  "30d": 30 * DAY,
  "90d": 90 * DAY,
};

/** Longest absolute range any explorer accepts. */
export const MAX_RANGE_MS = 90 * DAY;

export function windowMs(w: string): number | null {
  return RELATIVE[w] ?? null;
}

export type ResolvedRange = {
  startMs: number;
  endMs: number;
  /** The relative key ("1h") or "custom". */
  key: string;
  custom: boolean;
  /** Short human label: "last 1h" / "Sep 28 13:05 → 14:05 utc". */
  label: string;
};

type Params = { window?: string | string[]; from?: string | string[]; to?: string | string[] };

function first(v: string | string[] | undefined): string | undefined {
  return Array.isArray(v) ? v[0] : v;
}

/** Epoch ms from "1727520000000" or an ISO string; null when unparseable. */
export function parseInstant(v: string | undefined): number | null {
  if (!v) return null;
  const s = v.trim();
  if (/^\d{10,16}$/.test(s)) {
    const n = Number(s);
    // Accept seconds too (10 digits) — people paste both.
    return s.length <= 10 ? n * 1000 : n;
  }
  const t = Date.parse(s);
  return Number.isFinite(t) ? t : null;
}

export function resolveRange(
  sp: Params,
  opts: { defaultWindow: string; allowed?: readonly string[]; now?: number },
): ResolvedRange {
  const now = opts.now ?? Date.now();
  const from = parseInstant(first(sp.from));
  const to = parseInstant(first(sp.to));
  if (from !== null && to !== null && to > from && to - from <= MAX_RANGE_MS && from < now + 5 * MINUTE) {
    const end = Math.min(to, now + 5 * MINUTE);
    return { startMs: from, endMs: end, key: "custom", custom: true, label: customLabel(from, end) };
  }
  const w = first(sp.window);
  const allowed = opts.allowed ?? Object.keys(RELATIVE);
  const key = w && allowed.includes(w) && RELATIVE[w] ? w : opts.defaultWindow;
  const span = RELATIVE[key] ?? HOUR;
  return { startMs: now - span, endMs: now, key, custom: false, label: `last ${key}` };
}

const pad = (n: number) => String(n).padStart(2, "0");

function customLabel(from: number, to: number): string {
  const a = new Date(from);
  const b = new Date(to);
  const day = (d: Date) => `${d.toLocaleString("en-US", { month: "short", timeZone: "UTC" })} ${d.getUTCDate()}`;
  const hm = (d: Date) => `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
  const sameDay = day(a) === day(b);
  return `${day(a)} ${hm(a)} → ${sameDay ? "" : day(b) + " "}${hm(b)} utc`;
}

const STEPS = [
  1_000, 5_000, 10_000, 15_000, 30_000, MINUTE, 2 * MINUTE, 5 * MINUTE, 10 * MINUTE, 15 * MINUTE,
  30 * MINUTE, HOUR, 2 * HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR, DAY,
];

/** Smallest nice step that yields ≤ `buckets` buckets over the span. */
export function autoStep(spanMs: number, buckets = 60): number {
  const target = Math.max(1, spanMs / Math.max(1, buckets));
  return STEPS.find((s) => s >= target) ?? DAY;
}

/** Bucket starts [start, end) aligned to the step (UTC epoch alignment). */
export function bucketStarts(startMs: number, endMs: number, stepMs: number, cap = 2000): number[] {
  const out: number[] = [];
  const first = Math.floor(startMs / stepMs) * stepMs;
  for (let t = first; t < endMs && out.length < cap; t += stepMs) out.push(t);
  return out;
}

// ─── cost windows (GetAllocation / GetCostTimeseries vocabulary) ─────────

export const COST_WINDOWS = ["24h", "7d", "30d", "today", "yesterday", "week", "month", "lastmonth"] as const;
export type CostWindow = (typeof COST_WINDOWS)[number];

export function isCostWindow(v: unknown): v is CostWindow {
  return typeof v === "string" && (COST_WINDOWS as readonly string[]).includes(v);
}

export const COST_WINDOW_LABEL: Record<CostWindow, string> = {
  "24h": "last 24h",
  "7d": "last 7d",
  "30d": "last 30d",
  today: "today",
  yesterday: "yesterday",
  week: "this week",
  month: "month to date",
  lastmonth: "last month",
};

/** Calendar windows resolve in UTC — the control plane does the same. */
export function costWindowRange(w: CostWindow, now = Date.now()): { startMs: number; endMs: number } {
  const d = new Date(now);
  const midnight = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
  switch (w) {
    case "24h":
      return { startMs: now - DAY, endMs: now };
    case "7d":
      return { startMs: now - 7 * DAY, endMs: now };
    case "30d":
      return { startMs: now - 30 * DAY, endMs: now };
    case "today":
      return { startMs: midnight, endMs: now };
    case "yesterday":
      return { startMs: midnight - DAY, endMs: midnight };
    case "week": {
      const dow = (d.getUTCDay() + 6) % 7; // Monday = 0
      return { startMs: midnight - dow * DAY, endMs: now };
    }
    case "month":
      return { startMs: Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1), endMs: now };
    case "lastmonth":
      return {
        startMs: Date.UTC(d.getUTCFullYear(), d.getUTCMonth() - 1, 1),
        endMs: Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1),
      };
  }
}

/** Monthly-rate multiplier: $ over the window → $/month (730h). */
export function monthlyFactor(startMs: number, endMs: number): number {
  const hours = Math.max(1 / 60, (endMs - startMs) / HOUR);
  return 730 / hours;
}
