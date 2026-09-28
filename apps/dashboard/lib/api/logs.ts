// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// LogsService with the degrade-to-demo contract. One distinction matters
// here more than anywhere else: a query the control plane REJECTS
// (invalid_argument — a LogQL typo) is shown to the user as an error in
// the editor, never papered over with demo data. Only transport-level
// failures (unset / unreachable / unimplemented) fall back.

import "server-only";
import { demoLogLabels, demoLogPatterns, demoLogVolume, demoQueryLogs } from "@/lib/demo/logs";
import { LogQLError } from "@/lib/logql/parse";
import { alignSeries, toLogLines, toLogPatterns, toLogVolume, toStats } from "@/lib/logs/map";
import type {
  GetLogPatternsResponseJson,
  GetLogVolumeResponseJson,
  ListLogLabelsResponseJson,
  LogLine,
  LogPattern,
  LogVolume,
  MetricResult,
  QueryLogsResponseJson,
  QueryStats,
} from "@/lib/logs/types";
import { callUnary, type RpcError } from "./rpc";
import { demo, live, type Sourced } from "./source";
import { lineId } from "@/lib/logs/map";

const SERVICE = "kubehero.v1.LogsService";
/** $/GB-month the demo store prices log volume at (the control plane's default knob). */
export const DEMO_LOG_USD_PER_GB = 0.5;

export type QueryError = { message: string; pos?: number };
export type WithQueryError<T> = Sourced<T> & { queryError?: QueryError };

export type LinesResult = { kind: "streams"; lines: LogLine[]; stats: QueryStats | null };
export type MatrixResult = { kind: "matrix"; metric: MetricResult; stats: QueryStats | null };

type Range = { startMs: number; endMs: number };

function degradeOrReject<T>(err: RpcError, empty: T, fixture: () => T): WithQueryError<T> {
  if (err.code === "invalid_argument") return { ...live(empty), queryError: { message: err.message || "invalid query" } };
  if (err.code === "not_configured") return demoOrError(fixture, empty, "unset");
  return demoOrError(fixture, empty, "error", `${err.code}: ${err.message}`.slice(0, 160));
}

/** Run a demo evaluation; a demo parse error becomes a query error, not a crash. */
function demoOrError<T>(fixture: () => T, empty: T, reason: "unset" | "error" | "forced", detail?: string): WithQueryError<T> {
  try {
    return demo(fixture(), reason, detail);
  } catch (e) {
    if (e instanceof LogQLError) return { ...demo(empty, reason, detail), queryError: { message: e.message, pos: e.pos } };
    throw e;
  }
}

function demoLinesToView(lines: ReturnType<typeof demoQueryLogs> & { resultType: "streams" }): LogLine[] {
  return lines.lines.map((l) => ({
    id: lineId(l.tsNs, l.labels, l.body),
    tsMs: l.tsMs,
    tsNs: l.tsNs,
    body: l.body,
    level: l.level,
    labels: l.labels,
  }));
}

export async function queryLogs(q: {
  query: string;
  range: Range;
  limit: number;
  direction: "backward" | "forward";
  stepMs: number;
  clusterId?: string;
  forceDemo?: boolean;
  now?: number;
}): Promise<WithQueryError<LinesResult | MatrixResult>> {
  const now = q.now ?? Date.now();
  const empty: LinesResult = { kind: "streams", lines: [], stats: null };
  const fixture = (): LinesResult | MatrixResult => {
    const r = demoQueryLogs(q.query, { ...q.range, limit: q.limit, direction: q.direction, stepMs: q.stepMs, now });
    if (r.resultType === "streams") return { kind: "streams", lines: demoLinesToView(r), stats: null };
    return {
      kind: "matrix",
      metric: alignSeries(r.series.map((s) => ({ labels: s.labels, points: s.points.map((p) => ({ tsUnixMs: p.t, value: p.v })) }))),
      stats: null,
    };
  };
  if (q.forceDemo) return demoOrError(fixture, empty, "forced");
  const res = await callUnary<QueryLogsResponseJson>("cp", SERVICE, "QueryLogs", {
    query: q.query,
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    limit: q.limit,
    direction: q.direction,
    stepMs: String(q.stepMs),
    clusterId: q.clusterId ?? "",
  });
  if (!res.ok) return degradeOrReject<LinesResult | MatrixResult>(res.error, empty, fixture);
  const stats = toStats(res.data.stats);
  if (res.data.resultType === "matrix") {
    return live({ kind: "matrix", metric: alignSeries(res.data.series ?? []), stats });
  }
  const lines = toLogLines(res.data.lines, q.direction);
  const upstreamDemo = lines.length > 0 && lines.every((l) => l.labels.source === "demo");
  const out: LinesResult = { kind: "streams", lines, stats };
  return upstreamDemo ? demo(out, "upstream") : live(out);
}

export async function getLogVolume(q: {
  query: string;
  range: Range;
  stepMs: number;
  groupBy?: string;
  clusterId?: string;
  forceDemo?: boolean;
  now?: number;
}): Promise<WithQueryError<LogVolume>> {
  const now = q.now ?? Date.now();
  const groupBy = q.groupBy ?? "level";
  const empty: LogVolume = { times: [], stepMs: q.stepMs, series: [], totalLines: 0, totalBytes: 0, estCostUsdMonth: 0 };
  const fixture = (): LogVolume => {
    const v = demoLogVolume(q.query, { ...q.range, stepMs: q.stepMs, groupBy, now });
    const days = Math.max(1 / 1440, (q.range.endMs - q.range.startMs) / 86_400_000);
    return { ...v, estCostUsdMonth: ((v.totalBytes / days) * 30 * DEMO_LOG_USD_PER_GB) / 1e9 };
  };
  if (q.forceDemo) return demoOrError(fixture, empty, "forced");
  const res = await callUnary<GetLogVolumeResponseJson>("cp", SERVICE, "GetLogVolume", {
    query: q.query,
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    stepMs: String(q.stepMs),
    groupBy,
    clusterId: q.clusterId ?? "",
  });
  if (!res.ok) return degradeOrReject(res.error, empty, fixture);
  return live(toLogVolume(res.data, groupBy));
}

export async function getLogPatterns(q: {
  query: string;
  range: Range;
  limit?: number;
  clusterId?: string;
  forceDemo?: boolean;
  now?: number;
}): Promise<WithQueryError<{ patterns: LogPattern[]; linesAnalyzed: number }>> {
  const now = q.now ?? Date.now();
  const empty = { patterns: [] as LogPattern[], linesAnalyzed: 0 };
  const fixture = () => {
    const r = demoLogPatterns(q.query, { ...q.range, limit: q.limit ?? 50, now });
    return { linesAnalyzed: r.linesAnalyzed, patterns: r.patterns.map((p) => ({ ...p, trend: p.trend.map((x) => x.v) })) };
  };
  if (q.forceDemo) return demoOrError(fixture, empty, "forced");
  const res = await callUnary<GetLogPatternsResponseJson>("cp", SERVICE, "GetLogPatterns", {
    query: q.query,
    startUnixMs: String(Math.floor(q.range.startMs)),
    endUnixMs: String(Math.floor(q.range.endMs)),
    limit: q.limit ?? 50,
    clusterId: q.clusterId ?? "",
  });
  if (!res.ok) return degradeOrReject(res.error, empty, fixture);
  return live(toLogPatterns(res.data));
}

const LABEL_NAME_RE = /^[A-Za-z_][A-Za-z0-9_.]{0,127}$/;

export async function listLogLabels(q: {
  name?: string;
  query?: string;
  range?: Range;
  clusterId?: string;
  forceDemo?: boolean;
}): Promise<Sourced<{ names: string[]; values: string[] }>> {
  if (q.name && !LABEL_NAME_RE.test(q.name)) return live({ names: [], values: [] });
  const fixture = () => demoLogLabels(q.name, q.query);
  if (q.forceDemo) return demo(fixture(), "forced");
  const res = await callUnary<ListLogLabelsResponseJson>(
    "cp",
    SERVICE,
    "ListLogLabels",
    {
      name: q.name ?? "",
      query: q.query ?? "",
      startUnixMs: q.range ? String(Math.floor(q.range.startMs)) : "0",
      endUnixMs: q.range ? String(Math.floor(q.range.endMs)) : "0",
      clusterId: q.clusterId ?? "",
    },
    { timeoutMs: 5_000 },
  );
  if (!res.ok) {
    return res.error.code === "not_configured" ? demo(fixture(), "unset") : demo(fixture(), "error", res.error.code);
  }
  return live({ names: (res.data.names ?? []).slice(0, 500), values: (res.data.values ?? []).slice(0, 500) });
}
