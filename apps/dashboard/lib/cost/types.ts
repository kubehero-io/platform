// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// CostService wire shapes (Connect JSON of packages/proto/kubehero/v1/
// cost.proto) and the dashboard's view models. Wire types mark every
// field optional because protojson omits zero values; int64/uint64 fields
// are typed string | number because they arrive as strings.

export type I64 = string | number;

// ─── wire ────────────────────────────────────────────────────────────────

export type AllocationJson = {
  name?: string;
  properties?: Record<string, string>;
  startUnixMs?: I64;
  endUnixMs?: I64;
  minutes?: number;
  cpuCoreHours?: number;
  cpuCoreRequestAverage?: number;
  cpuCoreUsageAverage?: number;
  cpuCost?: number;
  cpuEfficiency?: number;
  ramByteHours?: number;
  ramByteRequestAverage?: number;
  ramByteUsageAverage?: number;
  ramCost?: number;
  ramEfficiency?: number;
  gpuHours?: number;
  gpuCost?: number;
  networkCost?: number;
  pvCost?: number;
  sharedCost?: number;
  idleCost?: number;
  totalCost?: number;
  totalEfficiency?: number;
  recoverableCost?: number;
  logIngestGb?: number;
};

export type GetAllocationRequest = {
  window: string;
  aggregate: string[];
  filters?: Record<string, string>;
  includeIdle?: boolean;
  shareIdle?: string;
  sharedNamespaces?: string[];
  clusterId?: string;
};

export type GetAllocationResponseJson = {
  allocations?: AllocationJson[];
  totals?: AllocationJson;
  startUnixMs?: I64;
  endUnixMs?: I64;
  source?: string;
};

export type PointJson = { tsUnixMs?: I64; value?: number };
export type SeriesJson = { labels?: Record<string, string>; points?: PointJson[] };

export type GetCostTimeseriesRequest = {
  window?: string;
  step?: string;
  groupBy?: string;
  filters?: Record<string, string>;
  top?: number;
  clusterId?: string;
};

export type GetCostTimeseriesResponseJson = {
  series?: SeriesJson[];
  totalUsd?: number;
  forecastMonthUsd?: number;
  source?: string;
};

export type RightsizingJson = {
  id?: string;
  cluster?: string;
  namespace?: string;
  workload?: string;
  workloadKind?: string;
  container?: string;
  replicas?: number;
  cpuRequestCores?: number;
  cpuLimitCores?: number;
  cpuP50Cores?: number;
  cpuP95Cores?: number;
  cpuP99Cores?: number;
  cpuMaxCores?: number;
  cpuRecommendedCores?: number;
  memRequestBytes?: I64;
  memLimitBytes?: I64;
  memP50Bytes?: I64;
  memP99Bytes?: I64;
  memMaxBytes?: I64;
  memRecommendedBytes?: I64;
  currentCostUsdMonth?: number;
  recommendedCostUsdMonth?: number;
  savingsUsdMonth?: number;
  samples?: I64;
  window?: string;
  confidence?: string;
  direction?: string;
  reason?: string;
  oomKills?: number;
  cpuThrottleRisk?: number;
};

export type ListRightsizingRequest = {
  clusterId?: string;
  namespace?: string;
  window?: string;
  headroomPct?: number;
  cpuPercentile?: string;
  minSavingsUsdMonth?: number;
  limit?: number;
};

export type ListRightsizingResponseJson = {
  recommendations?: RightsizingJson[];
  totalSavingsUsdMonth?: number;
  source?: string;
};

export type EfficiencyBreakdownJson = {
  name?: string;
  cpuEfficiency?: number;
  ramEfficiency?: number;
  idleCostUsdMonth?: number;
  totalCostUsdMonth?: number;
  score?: number;
};

export type GetEfficiencyResponseJson = {
  score?: number;
  cpuEfficiency?: number;
  ramEfficiency?: number;
  idleCostUsdMonth?: number;
  recoverableUsdMonth?: number;
  clusters?: EfficiencyBreakdownJson[];
  namespaces?: EfficiencyBreakdownJson[];
  source?: string;
};

// ─── view models ─────────────────────────────────────────────────────────

export type AllocationRow = {
  name: string;
  properties: Record<string, string>;
  cpuCost: number;
  ramCost: number;
  gpuCost: number;
  networkCost: number;
  pvCost: number;
  sharedCost: number;
  idleCost: number;
  totalCost: number;
  cpuEfficiency: number;
  ramEfficiency: number;
  totalEfficiency: number;
  recoverableCost: number;
  cpuCoreRequestAverage: number;
  cpuCoreUsageAverage: number;
  ramByteRequestAverage: number;
  ramByteUsageAverage: number;
  gpuHours: number;
  logIngestGb: number;
  /** The per-cluster "__idle__" row (unallocated node capacity). */
  isIdle: boolean;
};

export type AllocationView = {
  rows: AllocationRow[];
  totals: AllocationRow;
  startMs: number;
  endMs: number;
};

export type CostSeries = { key: string; label: string; values: number[] };

export type CostSeriesView = {
  /** Step starts (ms). */
  times: number[];
  stepMs: number;
  series: CostSeries[];
  totalUsd: number;
  forecastMonthUsd: number;
};

export type Rightsizing = {
  id: string;
  cluster: string;
  namespace: string;
  workload: string;
  workloadKind: string;
  container: string;
  replicas: number;
  cpu: { request: number; limit: number; p50: number; p95: number; p99: number; max: number; recommended: number };
  mem: { request: number; limit: number; p50: number; p99: number; max: number; recommended: number };
  currentCostUsdMonth: number;
  recommendedCostUsdMonth: number;
  savingsUsdMonth: number;
  samples: number;
  window: string;
  confidence: "low" | "medium" | "high";
  direction: "downsize" | "upsize" | "ok";
  reason: string;
  oomKills: number;
  cpuThrottleRisk: number;
};

export type RightsizingView = { recs: Rightsizing[]; totalSavingsUsdMonth: number };

export type EfficiencyBreakdown = {
  name: string;
  cpuEfficiency: number;
  ramEfficiency: number;
  idleCostUsdMonth: number;
  totalCostUsdMonth: number;
  score: number;
};

export type EfficiencyView = {
  score: number;
  cpuEfficiency: number;
  ramEfficiency: number;
  idleCostUsdMonth: number;
  recoverableUsdMonth: number;
  clusters: EfficiencyBreakdown[];
  namespaces: EfficiencyBreakdown[];
};

// ─── dimensions ──────────────────────────────────────────────────────────

export const AGGREGATES = ["namespace", "workload", "team", "cluster", "node", "nodepool", "zone", "controller", "cost_center"] as const;
export type Aggregate = (typeof AGGREGATES)[number] | `label:${string}`;

export const TIMESERIES_GROUPS = ["namespace", "team", "cluster", "nodepool", "workload"] as const;

/** Label keys must be Kubernetes label-ish; they end up in a query. */
export const LABEL_KEY_RE = /^[a-zA-Z0-9]([-a-zA-Z0-9_./]{0,120}[a-zA-Z0-9])?$/;

export function parseAggregate(v: string | undefined): Aggregate {
  if (!v) return "namespace";
  if ((AGGREGATES as readonly string[]).includes(v)) return v as Aggregate;
  if (v.startsWith("label:") && LABEL_KEY_RE.test(v.slice(6))) return v as Aggregate;
  return "namespace";
}
