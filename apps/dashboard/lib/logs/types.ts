// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// LogsService wire shapes (observe.proto, Connect JSON) and view models.

export type I64 = string | number;

export type PointJson = { tsUnixMs?: I64; value?: number };
export type SeriesJson = { labels?: Record<string, string>; points?: PointJson[] };

export type LogLineJson = {
  tsUnixNano?: I64;
  body?: string;
  level?: string;
  labels?: Record<string, string>;
  traceId?: string;
};

export type QueryLogsResponseJson = {
  resultType?: string;
  lines?: LogLineJson[];
  series?: SeriesJson[];
  stats?: { rowsScanned?: I64; bytesScanned?: I64; execMs?: number };
};

export type GetLogVolumeResponseJson = {
  series?: SeriesJson[];
  totalLines?: I64;
  totalBytes?: I64;
  estCostUsdMonth?: number;
};

export type LogPatternJson = {
  pattern?: string;
  count?: I64;
  level?: string;
  sharePct?: number;
  trend?: PointJson[];
  sample?: string;
};

export type GetLogPatternsResponseJson = { patterns?: LogPatternJson[]; linesAnalyzed?: I64 };
export type ListLogLabelsResponseJson = { names?: string[]; values?: string[] };
export type TailLogsResponseJson = { lines?: LogLineJson[]; dropped?: I64 };

// ─── view models (serialisable: they cross into client components) ─────

export type LogLine = {
  /** Stable identity: ns timestamp + stream + body hash. */
  id: string;
  tsMs: number;
  tsNs: string;
  body: string;
  level: string;
  labels: Record<string, string>;
  traceId?: string;
};

export type LogVolume = {
  times: number[];
  stepMs: number;
  series: { key: string; values: number[] }[];
  totalLines: number;
  totalBytes: number;
  estCostUsdMonth: number;
};

export type LogPattern = {
  pattern: string;
  count: number;
  level: string;
  sharePct: number;
  trend: number[];
  sample: string;
};

export type MetricSeries = { key: string; label: string; labels: Record<string, string>; values: (number | null)[] };

export type MetricResult = { times: number[]; stepMs: number; series: MetricSeries[] };

export type QueryStats = { rowsScanned: number; bytesScanned: number; execMs: number };
