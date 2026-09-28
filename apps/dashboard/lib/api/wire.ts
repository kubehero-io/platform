// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Readers for Connect JSON (protojson) values. The wire rules that bite:
//   · int64 / uint64 arrive as STRINGS ("1727500000123456789");
//   · zero values are OMITTED (0, "", false, [] simply aren't there);
//   · doubles may arrive as "NaN" / "Infinity" / "-Infinity" strings.
// Every mapper goes through these so a missing or oddly-typed field
// degrades to a sane default instead of NaN leaking into the UI.
// Pure and client-safe.

export function num(v: unknown, fallback = 0): number {
  if (typeof v === "number") return Number.isFinite(v) ? v : fallback;
  if (typeof v === "string" && v.trim() !== "") {
    const n = Number(v);
    return Number.isFinite(n) ? n : fallback;
  }
  return fallback;
}

/** int64 that fits a double (ms timestamps, counts, bytes < 2^53). */
export function int64(v: unknown, fallback = 0): number {
  return num(v, fallback);
}

export function str(v: unknown, fallback = ""): string {
  return typeof v === "string" ? v : fallback;
}

export function bool(v: unknown): boolean {
  return v === true;
}

export function arr<T>(v: T[] | undefined | null): T[] {
  return Array.isArray(v) ? v : [];
}

export function rec(v: Record<string, string> | undefined | null): Record<string, string> {
  if (!v || typeof v !== "object") return {};
  const out: Record<string, string> = {};
  for (const [k, val] of Object.entries(v)) if (typeof val === "string") out[k] = val;
  return out;
}

/**
 * Nanosecond int64 (string) → epoch milliseconds (float). 1.7e18 ns is
 * beyond Number's 2^53 exact range, so split the decimal string instead
 * of calling Number() on the whole thing: sub-millisecond digits are kept
 * as a fraction, nothing is rounded away at ms resolution.
 */
export function nsToMs(v: unknown): number {
  if (typeof v === "number") return Number.isFinite(v) ? v / 1e6 : 0;
  if (typeof v !== "string" || !/^-?\d+$/.test(v.trim())) return 0;
  const s = v.trim();
  const neg = s.startsWith("-");
  const digits = neg ? s.slice(1) : s;
  if (digits.length <= 6) return (neg ? -1 : 1) * (Number(digits) / 1e6);
  const ms = Number(digits.slice(0, -6)) + Number(digits.slice(-6)) / 1e6;
  return neg ? -ms : ms;
}

/** Canonical nanosecond string for identity/sorting without precision loss. */
export function nsKey(v: unknown): string {
  if (typeof v === "string" && /^\d+$/.test(v.trim())) return v.trim().replace(/^0+(?=\d)/, "");
  if (typeof v === "number" && Number.isFinite(v)) return BigInt(Math.round(v)).toString();
  return "0";
}

/** Compare two nanosecond strings numerically (no BigInt needed). */
export function compareNs(a: string, b: string): number {
  if (a.length !== b.length) return a.length - b.length;
  return a < b ? -1 : a > b ? 1 : 0;
}

/** epoch ms → nanosecond string (for request fields / resume cursors). */
export function msToNs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return "0";
  const whole = Math.floor(ms);
  const frac = Math.round((ms - whole) * 1e6);
  return `${whole}${String(frac).padStart(6, "0")}`;
}
