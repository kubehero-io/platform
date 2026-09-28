// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Deterministic randomness for demo fixtures: same seed → same numbers on
// every render, server and client, so screenshots and tests are stable.

/** FNV-1a 32-bit hash of a string. */
export function hash32(s: string): number {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

/** mulberry32 PRNG → floats in [0, 1). */
export function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

export function rngFor(...parts: (string | number)[]): () => number {
  return mulberry32(hash32(parts.join("|")));
}

/** Single deterministic value in [0,1) for a key — cheap, no state. */
export function unit(...parts: (string | number)[]): number {
  return mulberry32(hash32(parts.join("|")))();
}

export function pick<T>(r: () => number, xs: readonly T[]): T {
  return xs[Math.floor(r() * xs.length) % xs.length];
}

/** Poisson-ish integer with the given mean (Knuth for small, normal approx for large). */
export function poisson(r: () => number, mean: number): number {
  if (mean <= 0) return 0;
  if (mean > 40) {
    // Box–Muller normal approximation.
    const u = Math.max(1e-9, r());
    const v = r();
    const z = Math.sqrt(-2 * Math.log(u)) * Math.cos(2 * Math.PI * v);
    return Math.max(0, Math.round(mean + z * Math.sqrt(mean)));
  }
  const L = Math.exp(-mean);
  let k = 0;
  let p = 1;
  do {
    k++;
    p *= r();
  } while (p > L && k < 1000);
  return k - 1;
}

/** Hex string of n chars. */
export function hex(r: () => number, n: number): string {
  let s = "";
  for (let i = 0; i < n; i++) s += Math.floor(r() * 16).toString(16);
  return s;
}

const POD_ALPHABET = "bcdfghjklmnpqrstvwxz2456789";
export function podSuffix(r: () => number, n = 5): string {
  let s = "";
  for (let i = 0; i < n; i++) s += POD_ALPHABET[Math.floor(r() * POD_ALPHABET.length)];
  return s;
}
