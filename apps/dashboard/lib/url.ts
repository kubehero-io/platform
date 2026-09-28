// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Tiny helpers for URL-state views: build links that change one or two
// search params and keep the rest (the time range, filters, sort…).

export type SearchParamsRecord = Record<string, string | string[] | undefined>;

/** Flatten Next's searchParams object to single string values. */
export function flatParams(sp: SearchParamsRecord): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(sp)) {
    const s = Array.isArray(v) ? v[0] : v;
    if (typeof s === "string" && s !== "") out[k] = s;
  }
  return out;
}

/** `pathname?…` with `patch` applied (null/"" deletes a key). */
export function hrefWith(
  pathname: string,
  current: Record<string, string>,
  patch: Record<string, string | number | null | undefined>,
): string {
  const next = new URLSearchParams(current);
  for (const [k, v] of Object.entries(patch)) {
    if (v === null || v === undefined || v === "") next.delete(k);
    else next.set(k, String(v));
  }
  const qs = next.toString();
  return qs ? `${pathname}?${qs}` : pathname;
}

/** First value of a search param, trimmed and length-capped (untrusted input). */
export function param(sp: SearchParamsRecord, key: string, max = 2048): string {
  const v = sp[key];
  const s = Array.isArray(v) ? v[0] : v;
  return typeof s === "string" ? s.trim().slice(0, max) : "";
}

/** Workload hub link — every workload name in the product points here. */
export function workloadHref(cluster: string, namespace: string, workload: string): string {
  return `/workloads/${encodeURIComponent(cluster)}/${encodeURIComponent(namespace)}/${encodeURIComponent(workload)}`;
}

/** /logs deep link with a LogQL query and optional relative window. */
export function logsHref(query: string, opts: { window?: string; tab?: string } = {}): string {
  const p = new URLSearchParams({ q: query });
  if (opts.window) p.set("window", opts.window);
  if (opts.tab) p.set("tab", opts.tab);
  return `/logs?${p.toString()}`;
}

/** Escape a string for use inside a LogQL double-quoted literal. */
export function logqlString(v: string): string {
  return `"${v.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
}

/** {namespace="x", workload="y"} */
export function logqlSelector(labels: Record<string, string | undefined>): string {
  const parts = Object.entries(labels)
    .filter(([, v]) => v)
    .map(([k, v]) => `${k}=${logqlString(v!)}`);
  return `{${parts.join(", ")}}`;
}
