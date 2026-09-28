// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// URL-driven table sorting (?sort=<column>&dir=asc|desc). Server pages
// sort the rows they render; <SortHeader> only edits the URL — so a
// sorted view is a shareable link and needs no client state.

export type SortDir = "asc" | "desc";
export type SortState<K extends string> = { key: K; dir: SortDir };

export function parseSort<K extends string>(
  sp: { sort?: string | string[]; dir?: string | string[] },
  allowed: readonly K[],
  fallback: SortState<K>,
): SortState<K> {
  const s = Array.isArray(sp.sort) ? sp.sort[0] : sp.sort;
  const d = Array.isArray(sp.dir) ? sp.dir[0] : sp.dir;
  const key = s && (allowed as readonly string[]).includes(s) ? (s as K) : fallback.key;
  const dir: SortDir = d === "asc" || d === "desc" ? d : key === fallback.key ? fallback.dir : "desc";
  return { key, dir };
}

type Accessor<T> = (row: T) => number | string | null | undefined;

/** Stable sort; nulls/NaN always last regardless of direction. */
export function sortRows<T, K extends string>(
  rows: readonly T[],
  state: SortState<K>,
  accessors: Record<K, Accessor<T>>,
): T[] {
  const get = accessors[state.key];
  const sign = state.dir === "asc" ? 1 : -1;
  return rows
    .map((row, i) => ({ row, i, v: get(row) }))
    .sort((a, b) => {
      const an = a.v === null || a.v === undefined || (typeof a.v === "number" && Number.isNaN(a.v));
      const bn = b.v === null || b.v === undefined || (typeof b.v === "number" && Number.isNaN(b.v));
      if (an || bn) return an === bn ? a.i - b.i : an ? 1 : -1;
      if (typeof a.v === "number" && typeof b.v === "number") return (a.v - b.v) * sign || a.i - b.i;
      return String(a.v).localeCompare(String(b.v)) * sign || a.i - b.i;
    })
    .map((x) => x.row);
}

/** Case-insensitive "all terms appear somewhere in the row" filter. */
export function matchesQuery(q: string, ...fields: (string | undefined | null)[]): boolean {
  const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (terms.length === 0) return true;
  const hay = fields.filter(Boolean).join(" ").toLowerCase();
  return terms.every((t) => hay.includes(t));
}
