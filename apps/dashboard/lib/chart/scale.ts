// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The dashboard's charting layer is deliberately tiny: a handful of SVG
// primitives (components/charts/*) over the pure math in this file. No
// chart library — the heaviest candidates cost more first-load JS than
// every page's own code, and none of them match the hairline/mono look.
//
// Everything here is DOM-free and unit-tested.

export type Domain = [number, number];

export function linear(domain: Domain, range: Domain): (v: number) => number {
  const [d0, d1] = domain;
  const [r0, r1] = range;
  const span = d1 - d0 || 1;
  return (v) => r0 + ((v - d0) / span) * (r1 - r0);
}

/** "Nice" step for ~count ticks across span: 1, 2, 2.5, 5 × 10^k. */
export function niceStep(span: number, count: number): number {
  if (!(span > 0) || !(count > 0)) return 1;
  const raw = span / count;
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  const f = raw / pow;
  const nice = f <= 1 ? 1 : f <= 2 ? 2 : f <= 2.5 ? 2.5 : f <= 5 ? 5 : 10;
  return nice * pow;
}

/** Ticks from 0 (or min) to a nice max ≥ max. Returns the ticks and the padded max. */
export function niceTicks(min: number, max: number, count = 4): { ticks: number[]; max: number; min: number } {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return { ticks: [0, 1], min: 0, max: 1 };
  if (max <= min) max = min + (Math.abs(min) || 1);
  const step = niceStep(max - min, count);
  const lo = Math.floor(min / step) * step;
  const hi = Math.ceil(max / step) * step;
  const ticks: number[] = [];
  // Guard against float drift and runaway loops.
  for (let v = lo, i = 0; v <= hi + step / 1e6 && i < 50; v += step, i++) {
    ticks.push(Number(v.toPrecision(12)));
  }
  return { ticks, min: lo, max: hi };
}

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;

/** Candidate tick intervals for time axes, smallest first. */
export const TIME_STEPS = [
  10_000, 30_000, MIN, 5 * MIN, 10 * MIN, 15 * MIN, 30 * MIN, HOUR, 3 * HOUR, 6 * HOUR, 12 * HOUR,
  DAY, 2 * DAY, 7 * DAY, 14 * DAY, 30 * DAY,
];

/** Time ticks aligned to round wall-clock boundaries (UTC) with ≤ maxTicks. */
export function timeTicks(startMs: number, endMs: number, maxTicks = 6): { ticks: number[]; step: number } {
  const span = Math.max(1, endMs - startMs);
  const step = TIME_STEPS.find((s) => span / s <= maxTicks) ?? TIME_STEPS[TIME_STEPS.length - 1];
  const first = Math.ceil(startMs / step) * step;
  const ticks: number[] = [];
  for (let t = first; t <= endMs && ticks.length < 100; t += step) ticks.push(t);
  return { ticks, step };
}

const pad2 = (n: number) => String(n).padStart(2, "0");

/** Tick label appropriate for the tick interval (UTC so SSR == CSR). */
export function formatTimeTick(ms: number, step: number): string {
  const d = new Date(ms);
  const hm = `${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}`;
  if (step < MIN) return `${hm}:${pad2(d.getUTCSeconds())}`;
  if (step < DAY) return hm;
  return `${d.toLocaleString("en-US", { month: "short", timeZone: "UTC" })} ${d.getUTCDate()}`;
}

/** Full timestamp for tooltips: "Sep 28 14:05 utc". */
export function formatTimeFull(ms: number, withSeconds = false): string {
  const d = new Date(ms);
  const base = `${d.toLocaleString("en-US", { month: "short", timeZone: "UTC" })} ${d.getUTCDate()} ${pad2(d.getUTCHours())}:${pad2(d.getUTCMinutes())}`;
  return `${base}${withSeconds ? `:${pad2(d.getUTCSeconds())}` : ""} utc`;
}

/** Compact number: 1,284 / 12.9k / 4.2M. */
export function formatCompact(v: number, digits = 1): string {
  if (!Number.isFinite(v)) return "—";
  const a = Math.abs(v);
  const s = v < 0 ? "−" : "";
  if (a >= 1e9) return `${s}${trim((a / 1e9).toFixed(digits))}B`;
  if (a >= 1e6) return `${s}${trim((a / 1e6).toFixed(digits))}M`;
  if (a >= 1e4) return `${s}${trim((a / 1e3).toFixed(digits))}k`;
  if (a >= 100) return `${s}${Math.round(a).toLocaleString("en-US")}`;
  if (a >= 1) return `${s}${trim(a.toFixed(digits))}`;
  if (a === 0) return "0";
  return `${s}${trim(a.toPrecision(2))}`;
}

function trim(s: string): string {
  return s.includes(".") ? s.replace(/\.?0+$/, "") : s;
}

/** "$18.2k", "$640", "$0.42" — money in the dashboard's house style. */
export function formatUsd(v: number, opts: { cents?: boolean } = {}): string {
  if (!Number.isFinite(v)) return "—";
  const a = Math.abs(v);
  const s = v < 0 ? "−" : "";
  if (a >= 1e6) return `${s}$${trim((a / 1e6).toFixed(2))}M`;
  if (a >= 1e4) return `${s}$${trim((a / 1e3).toFixed(1))}k`;
  if (a >= 1000) return `${s}$${Math.round(a).toLocaleString("en-US")}`;
  if (a >= 10 || !opts.cents) return `${s}$${a >= 10 ? Math.round(a) : trim(a.toFixed(2))}`;
  return `${s}$${a.toFixed(2)}`;
}

/** Bytes in IEC units: "512 B", "6.0 GiB". */
export function formatBytes(v: number, digits = 1): string {
  if (!Number.isFinite(v) || v < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];
  let i = 0;
  let x = v;
  while (x >= 1024 && i < units.length - 1) {
    x /= 1024;
    i++;
  }
  return `${i === 0 ? Math.round(x) : x.toFixed(digits)} ${units[i]}`;
}

/** Decimal GB for transfer/ingest (what cloud bills use). */
export function formatGB(v: number): string {
  if (!Number.isFinite(v)) return "—";
  if (v >= 1000) return `${trim((v / 1000).toFixed(2))} TB`;
  if (v >= 10) return `${Math.round(v)} GB`;
  return `${trim(v.toFixed(2))} GB`;
}

/** CPU cores: "16", "2.5", "410m". */
export function formatCores(v: number): string {
  if (!Number.isFinite(v)) return "—";
  if (v === 0) return "0";
  if (v < 1) return `${Math.round(v * 1000)}m`;
  return trim(v.toFixed(v >= 10 ? 0 : 2));
}

export function formatPct(ratio: number, digits = 0): string {
  if (!Number.isFinite(ratio)) return "—";
  return `${(ratio * 100).toFixed(digits)}%`;
}

/** Nanoseconds of CPU → "1.2 s", "340 ms". */
export function formatDurationNs(ns: number): string {
  if (!Number.isFinite(ns)) return "—";
  const a = Math.abs(ns);
  if (a >= 3.6e12) return `${trim((ns / 3.6e12).toFixed(1))} h`;
  if (a >= 6e10) return `${trim((ns / 6e10).toFixed(1))} min`;
  if (a >= 1e9) return `${trim((ns / 1e9).toFixed(2))} s`;
  if (a >= 1e6) return `${trim((ns / 1e6).toFixed(1))} ms`;
  if (a >= 1e3) return `${trim((ns / 1e3).toFixed(1))} µs`;
  return `${Math.round(ns)} ns`;
}

/** Human duration from ms: "4h 12m", "22m", "38s". */
export function formatAgo(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return "—";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 48) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}
