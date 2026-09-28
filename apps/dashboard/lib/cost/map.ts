// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// CostService JSON → view models. Pure; unit-tested with protojson-shaped
// fixtures (int64 as strings, omitted zeros).

import { arr, int64, num, rec, str } from "@/lib/api/wire";
import type {
  AllocationJson,
  AllocationRow,
  AllocationView,
  CostSeriesView,
  EfficiencyBreakdown,
  EfficiencyBreakdownJson,
  EfficiencyView,
  GetAllocationResponseJson,
  GetCostTimeseriesResponseJson,
  GetEfficiencyResponseJson,
  ListRightsizingResponseJson,
  Rightsizing,
  RightsizingJson,
  RightsizingView,
} from "./types";

export const IDLE_NAME = "__idle__";

export function isIdleName(name: string): boolean {
  return name === IDLE_NAME || name.startsWith(`${IDLE_NAME}/`) || name.endsWith(`/${IDLE_NAME}`);
}

export function toAllocationRow(a: AllocationJson): AllocationRow {
  const name = str(a.name, "(unallocated)");
  const cpuCost = num(a.cpuCost);
  const ramCost = num(a.ramCost);
  const gpuCost = num(a.gpuCost);
  const networkCost = num(a.networkCost);
  const pvCost = num(a.pvCost);
  const sharedCost = num(a.sharedCost);
  const idleCost = num(a.idleCost);
  const parts = cpuCost + ramCost + gpuCost + networkCost + pvCost + sharedCost + idleCost;
  return {
    name,
    properties: rec(a.properties),
    cpuCost,
    ramCost,
    gpuCost,
    networkCost,
    pvCost,
    sharedCost,
    idleCost,
    // Trust the server's total; derive it only when omitted (== 0).
    totalCost: num(a.totalCost) || parts,
    cpuEfficiency: num(a.cpuEfficiency),
    ramEfficiency: num(a.ramEfficiency),
    totalEfficiency: num(a.totalEfficiency),
    recoverableCost: num(a.recoverableCost),
    cpuCoreRequestAverage: num(a.cpuCoreRequestAverage),
    cpuCoreUsageAverage: num(a.cpuCoreUsageAverage),
    ramByteRequestAverage: num(a.ramByteRequestAverage),
    ramByteUsageAverage: num(a.ramByteUsageAverage),
    gpuHours: num(a.gpuHours),
    logIngestGb: num(a.logIngestGb),
    isIdle: isIdleName(name),
  };
}

export function sumRows(rows: AllocationRow[], name = "total"): AllocationRow {
  const t: AllocationRow = {
    name,
    properties: {},
    cpuCost: 0,
    ramCost: 0,
    gpuCost: 0,
    networkCost: 0,
    pvCost: 0,
    sharedCost: 0,
    idleCost: 0,
    totalCost: 0,
    cpuEfficiency: 0,
    ramEfficiency: 0,
    totalEfficiency: 0,
    recoverableCost: 0,
    cpuCoreRequestAverage: 0,
    cpuCoreUsageAverage: 0,
    ramByteRequestAverage: 0,
    ramByteUsageAverage: 0,
    gpuHours: 0,
    logIngestGb: 0,
    isIdle: false,
  };
  let cpuW = 0;
  let ramW = 0;
  let totW = 0;
  for (const r of rows) {
    t.cpuCost += r.cpuCost;
    t.ramCost += r.ramCost;
    t.gpuCost += r.gpuCost;
    t.networkCost += r.networkCost;
    t.pvCost += r.pvCost;
    t.sharedCost += r.sharedCost;
    t.idleCost += r.idleCost;
    t.totalCost += r.totalCost;
    t.recoverableCost += r.recoverableCost;
    t.cpuCoreRequestAverage += r.cpuCoreRequestAverage;
    t.cpuCoreUsageAverage += r.cpuCoreUsageAverage;
    t.ramByteRequestAverage += r.ramByteRequestAverage;
    t.ramByteUsageAverage += r.ramByteUsageAverage;
    t.gpuHours += r.gpuHours;
    t.logIngestGb += r.logIngestGb;
    if (!r.isIdle) {
      cpuW += r.cpuCost * r.cpuEfficiency;
      ramW += r.ramCost * r.ramEfficiency;
      totW += (r.cpuCost + r.ramCost) * r.totalEfficiency;
    }
  }
  // Cost-weighted efficiencies, the way OpenCost blends them.
  const allocCpu = rows.filter((r) => !r.isIdle).reduce((s, r) => s + r.cpuCost, 0);
  const allocRam = rows.filter((r) => !r.isIdle).reduce((s, r) => s + r.ramCost, 0);
  t.cpuEfficiency = allocCpu > 0 ? cpuW / allocCpu : 0;
  t.ramEfficiency = allocRam > 0 ? ramW / allocRam : 0;
  t.totalEfficiency = allocCpu + allocRam > 0 ? totW / (allocCpu + allocRam) : 0;
  return t;
}

export function toAllocationView(res: GetAllocationResponseJson, fallbackRange: { startMs: number; endMs: number }): AllocationView {
  const rows = arr(res.allocations).map(toAllocationRow);
  const totals = res.totals ? toAllocationRow({ ...res.totals, name: "total" }) : sumRows(rows);
  totals.isIdle = false;
  return {
    rows,
    totals,
    startMs: int64(res.startUnixMs) || fallbackRange.startMs,
    endMs: int64(res.endUnixMs) || fallbackRange.endMs,
  };
}

/** Series from GetCostTimeseries → aligned arrays over the union of timestamps. */
export function toCostSeriesView(res: GetCostTimeseriesResponseJson, groupBy: string): CostSeriesView {
  const series = arr(res.series);
  const stamps = new Set<number>();
  for (const s of series) for (const p of arr(s.points)) stamps.add(int64(p.tsUnixMs));
  const times = [...stamps].filter((t) => t > 0).sort((a, b) => a - b);
  const index = new Map(times.map((t, i) => [t, i]));
  const out = series.map((s, i) => {
    const labels = rec(s.labels);
    const label = (groupBy && labels[groupBy]) || labels.name || Object.values(labels)[0] || (series.length === 1 ? "total" : `series ${i + 1}`);
    const values = new Array<number>(times.length).fill(0);
    for (const p of arr(s.points)) {
      const at = index.get(int64(p.tsUnixMs));
      if (at !== undefined) values[at] += num(p.value);
    }
    return { key: label, label, values };
  });
  // Largest first; "other" always last.
  const total = (v: number[]) => v.reduce((a, b) => a + b, 0);
  out.sort((a, b) => (a.key === "other" ? 1 : b.key === "other" ? -1 : total(b.values) - total(a.values)));
  const stepMs = times.length > 1 ? Math.min(...times.slice(1).map((t, i) => t - times[i])) : 86_400_000;
  return {
    times,
    stepMs,
    series: out,
    totalUsd: num(res.totalUsd) || out.reduce((s, x) => s + total(x.values), 0),
    forecastMonthUsd: num(res.forecastMonthUsd),
  };
}

const CONF = new Set(["low", "medium", "high"]);
const DIR = new Set(["downsize", "upsize", "ok"]);

export function toRightsizing(r: RightsizingJson): Rightsizing {
  const cluster = str(r.cluster);
  const namespace = str(r.namespace);
  const workload = str(r.workload);
  const container = str(r.container);
  const conf = str(r.confidence);
  const dir = str(r.direction);
  return {
    id: str(r.id) || `${cluster}/${namespace}/${workload}/${container}`,
    cluster,
    namespace,
    workload,
    workloadKind: str(r.workloadKind, "Deployment"),
    container,
    replicas: Math.max(1, num(r.replicas, 1)),
    cpu: {
      request: num(r.cpuRequestCores),
      limit: num(r.cpuLimitCores),
      p50: num(r.cpuP50Cores),
      p95: num(r.cpuP95Cores),
      p99: num(r.cpuP99Cores),
      max: num(r.cpuMaxCores),
      recommended: num(r.cpuRecommendedCores),
    },
    mem: {
      request: int64(r.memRequestBytes),
      limit: int64(r.memLimitBytes),
      p50: int64(r.memP50Bytes),
      p99: int64(r.memP99Bytes),
      max: int64(r.memMaxBytes),
      recommended: int64(r.memRecommendedBytes),
    },
    currentCostUsdMonth: num(r.currentCostUsdMonth),
    recommendedCostUsdMonth: num(r.recommendedCostUsdMonth),
    savingsUsdMonth: num(r.savingsUsdMonth),
    samples: int64(r.samples),
    window: str(r.window, "7d"),
    confidence: (CONF.has(conf) ? conf : "low") as Rightsizing["confidence"],
    direction: (DIR.has(dir) ? dir : "ok") as Rightsizing["direction"],
    reason: str(r.reason),
    oomKills: num(r.oomKills),
    cpuThrottleRisk: num(r.cpuThrottleRisk),
  };
}

export function toRightsizingView(res: ListRightsizingResponseJson): RightsizingView {
  const recs = arr(res.recommendations).map(toRightsizing);
  return {
    recs,
    totalSavingsUsdMonth: num(res.totalSavingsUsdMonth) || recs.reduce((s, r) => s + Math.max(0, r.savingsUsdMonth), 0),
  };
}

function toBreakdown(b: EfficiencyBreakdownJson): EfficiencyBreakdown {
  return {
    name: str(b.name),
    cpuEfficiency: num(b.cpuEfficiency),
    ramEfficiency: num(b.ramEfficiency),
    idleCostUsdMonth: num(b.idleCostUsdMonth),
    totalCostUsdMonth: num(b.totalCostUsdMonth),
    score: num(b.score),
  };
}

export function toEfficiencyView(res: GetEfficiencyResponseJson): EfficiencyView {
  return {
    score: Math.max(0, Math.min(100, num(res.score))),
    cpuEfficiency: num(res.cpuEfficiency),
    ramEfficiency: num(res.ramEfficiency),
    idleCostUsdMonth: num(res.idleCostUsdMonth),
    recoverableUsdMonth: num(res.recoverableUsdMonth),
    clusters: arr(res.clusters).map(toBreakdown),
    namespaces: arr(res.namespaces).map(toBreakdown),
  };
}
