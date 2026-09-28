// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// LogsService JSON → view models. Pure; tested with protojson fixtures
// (nanosecond int64 strings, omitted zeros).

import { arr, compareNs, int64, nsKey, nsToMs, num, rec, str } from "@/lib/api/wire";
import { hash32 } from "@/lib/demo/rng";
import type {
  GetLogPatternsResponseJson,
  GetLogVolumeResponseJson,
  LogLine,
  LogLineJson,
  LogPattern,
  LogVolume,
  MetricResult,
  QueryStats,
  SeriesJson,
} from "./types";

export function lineId(tsNs: string, labels: Record<string, string>, body: string): string {
  const stream = `${labels.namespace ?? ""}/${labels.pod ?? ""}/${labels.container ?? ""}`;
  return `${tsNs}:${hash32(stream).toString(36)}:${hash32(body).toString(36)}`;
}

export function toLogLine(l: LogLineJson): LogLine {
  const tsNs = nsKey(l.tsUnixNano);
  const labels = rec(l.labels);
  const body = str(l.body);
  return {
    id: lineId(tsNs, labels, body),
    tsMs: nsToMs(l.tsUnixNano),
    tsNs,
    body,
    level: str(l.level) || labels.level || labels.detected_level || "",
    labels,
    traceId: str(l.traceId) || undefined,
  };
}

/** Map, de-duplicate (same id) and order lines. */
export function toLogLines(lines: LogLineJson[] | undefined, direction: "backward" | "forward" = "backward"): LogLine[] {
  const seen = new Set<string>();
  const out: LogLine[] = [];
  for (const l of arr(lines)) {
    const v = toLogLine(l);
    if (seen.has(v.id)) continue;
    seen.add(v.id);
    out.push(v);
  }
  out.sort((a, b) => (direction === "backward" ? compareNs(b.tsNs, a.tsNs) : compareNs(a.tsNs, b.tsNs)));
  return out;
}

/** Union of timestamps across series, each series aligned (null = no sample). */
export function alignSeries(series: SeriesJson[], labelKey = (l: Record<string, string>) => seriesLabel(l)): MetricResult {
  const stamps = new Set<number>();
  for (const s of series) for (const p of arr(s.points)) stamps.add(int64(p.tsUnixMs));
  const times = [...stamps].filter((t) => t > 0).sort((a, b) => a - b);
  const idx = new Map(times.map((t, i) => [t, i]));
  const out = series.map((s) => {
    const labels = rec(s.labels);
    const values = new Array<number | null>(times.length).fill(null);
    for (const p of arr(s.points)) {
      const i = idx.get(int64(p.tsUnixMs));
      if (i !== undefined) values[i] = num(p.value);
    }
    const label = labelKey(labels);
    return { key: label, label, labels, values };
  });
  const diffs = times.slice(1).map((t, i) => t - times[i]).filter((d) => d > 0);
  return { times, stepMs: diffs.length ? Math.min(...diffs) : 60_000, series: out };
}

/** `{namespace="shop", pod="x"}` → "shop · x" (values only, stable key order). */
export function seriesLabel(labels: Record<string, string>): string {
  const keys = Object.keys(labels).sort();
  if (keys.length === 0) return "{}";
  return keys.map((k) => labels[k]).join(" · ");
}

export function toLogVolume(res: GetLogVolumeResponseJson, groupBy = "level"): LogVolume {
  const aligned = alignSeries(arr(res.series), (l) => l[groupBy] || l.level || seriesLabel(l));
  return {
    times: aligned.times,
    stepMs: aligned.stepMs,
    series: aligned.series.map((s) => ({ key: s.key, values: s.values.map((v) => v ?? 0) })),
    totalLines: int64(res.totalLines),
    totalBytes: int64(res.totalBytes),
    estCostUsdMonth: num(res.estCostUsdMonth),
  };
}

export function toLogPatterns(res: GetLogPatternsResponseJson): { patterns: LogPattern[]; linesAnalyzed: number } {
  const patterns = arr(res.patterns).map((p) => ({
    pattern: str(p.pattern),
    count: int64(p.count),
    level: str(p.level),
    sharePct: num(p.sharePct),
    trend: arr(p.trend)
      .slice()
      .sort((a, b) => int64(a.tsUnixMs) - int64(b.tsUnixMs))
      .map((x) => num(x.value)),
    sample: str(p.sample),
  }));
  return { patterns, linesAnalyzed: int64(res.linesAnalyzed) };
}

export function toStats(s: { rowsScanned?: string | number; bytesScanned?: string | number; execMs?: number } | undefined): QueryStats | null {
  if (!s) return null;
  return { rowsScanned: int64(s.rowsScanned), bytesScanned: int64(s.bytesScanned), execMs: num(s.execMs) };
}

/** Pretty JSON if the body is a JSON object, else null. */
export function prettyJson(body: string): string | null {
  const t = body.trim();
  if (!t.startsWith("{") || t.length > 64 * 1024) return null;
  try {
    const v = JSON.parse(t) as unknown;
    return v && typeof v === "object" ? JSON.stringify(v, null, 2) : null;
  } catch {
    return null;
  }
}
