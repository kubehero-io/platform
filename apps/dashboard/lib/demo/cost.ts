// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo CostService: allocation, cost series, rightsizing and efficiency
// computed from the demo world (lib/demo/world.ts) with the same
// semantics the control plane implements — so every control on
// /allocation and /rightsizing does something real in demo mode.

import { allocate, sumRows, type AllocAtom } from "@/lib/cost/allocation";
import type {
  Aggregate,
  AllocationView,
  CostSeriesView,
  EfficiencyBreakdown,
  EfficiencyView,
  Rightsizing,
  RightsizingView,
} from "@/lib/cost/types";
import { CLUSTERS } from "@/lib/fleet-data";
import { costWindowRange, DAY, HOUR, isCostWindow, type CostWindow } from "@/lib/time-range";
import { hash32, unit } from "./rng";
import { clusterMonthlyUsd, demoWorkloads, totalUsd, type DemoContainer, type DemoWorkload } from "./world";

const GIB = 1024 ** 3;
const MONTH_H = 730;

// ─── nodes ──────────────────────────────────────────────────────────────

function nodeName(cluster: string, nodepool: string, i: number): string {
  const h = hash32(`${cluster}/${nodepool}/${i}`).toString(16).slice(0, 5);
  if (cluster.startsWith("eks")) {
    const n = hash32(`${cluster}${nodepool}${i}`);
    return `ip-10-${(n >> 8) & 63}-${(n >> 16) & 255}-${n & 255}.ec2.internal`;
  }
  if (cluster.startsWith("aks")) return `aks-${nodepool.replace(/[^a-z0-9]/g, "")}-${h}-vmss00000${i}`;
  return `gke-${cluster}-${nodepool}-${h}-${i}`;
}

// ─── rightsizing ────────────────────────────────────────────────────────

export type RightsizingOptions = {
  headroomPct?: number;
  cpuPercentile?: "p90" | "p95" | "p99" | "max";
  window?: string;
  clusterId?: string;
  namespace?: string;
  minSavingsUsdMonth?: number;
};

const roundTo = (v: number, step: number) => Math.round(v / step) * step;
const MI = 1024 * 1024;

function pctValue(c: DemoContainer, p: string): number {
  switch (p) {
    case "p90":
      return c.cpuP50 + (c.cpuP95 - c.cpuP50) * 0.8;
    case "p99":
      return c.cpuP99;
    case "max":
      return c.cpuMax;
    default:
      return c.cpuP95;
  }
}

/** The percentile engine, as specified for the control plane (cost.proto). */
export function recommend(w: DemoWorkload, c: DemoContainer, opts: RightsizingOptions = {}): Rightsizing {
  const headroom = (opts.headroomPct ?? 15) / 100;
  const pct = opts.cpuPercentile ?? "p95";
  const cpuRec = Math.max(0.01, roundTo(pctValue(c, pct) * (1 + headroom), 0.005));
  const memReq = c.memReqGiB * GIB;
  const memMax = c.memMaxGiB * GIB;
  const memP99 = c.memP99GiB * GIB;
  let memRec = Math.max(32 * MI, roundTo(Math.max(memP99, memMax) * (1 + headroom), MI));
  memRec = Math.max(memRec, memMax); // never below observed max
  if (c.oomKills > 0) {
    // OOM is not a trade-off: never shrink, and grow past the old limit.
    const floor = Math.max(memMax, c.memLimGiB * GIB, memReq) * 1.25;
    memRec = Math.max(memRec, roundTo(floor, MI));
  }

  const cpuPrice = w.cpuUsd / Math.max(0.001, c.cpuReq * w.replicas * w.containers.length);
  const memPrice = w.ramUsd / Math.max(0.001, c.memReqGiB * w.replicas * w.containers.length);
  const current = (c.cpuReq * cpuPrice + c.memReqGiB * memPrice) * w.replicas;
  const recommended = (cpuRec * cpuPrice + (memRec / GIB) * memPrice) * w.replicas;
  const savings = current - recommended;

  const cpuDown = cpuRec < c.cpuReq * 0.9;
  const cpuUp = cpuRec > c.cpuReq * 1.1;
  const memDown = memRec < memReq * 0.9;
  const memUp = memRec > memReq * 1.1;
  const direction = cpuUp || memUp ? "upsize" : cpuDown || memDown ? "downsize" : "ok";
  const window = opts.window ?? "7d";
  const days = Math.min(w.historyDays, window === "30d" ? 30 : window === "24h" ? 1 : 7);
  const confidence = days < 1 ? "low" : days < 5 ? "medium" : "high";

  const reasons: string[] = [];
  if (c.oomKills > 0) {
    reasons.push(
      `${c.oomKills} OOM kill${c.oomKills === 1 ? "" : "s"} in the window — memory must go up, not down (to ${(memRec / GIB).toFixed(2)} GiB, 25% over the ${c.memLimGiB} GiB limit).`,
    );
  }
  if (cpuDown) {
    reasons.push(
      `CPU requests ${fmtCores(c.cpuReq)} but ${pct} usage is ${fmtCores(pctValue(c, pct))}; sizing to ${pct} + ${Math.round(headroom * 100)}% headroom gives ${fmtCores(cpuRec)}.`,
    );
  } else if (cpuUp) {
    reasons.push(`CPU ${pct} (${fmtCores(pctValue(c, pct))}) runs above the ${fmtCores(c.cpuReq)} request — raise it to ${fmtCores(cpuRec)}.`);
  }
  if (memDown && c.oomKills === 0) {
    reasons.push(
      `Memory requests ${c.memReqGiB} GiB; the observed max is ${c.memMaxGiB.toFixed(2)} GiB, so ${(memRec / GIB).toFixed(2)} GiB (max + ${Math.round(headroom * 100)}%) is safe.`,
    );
  }
  if (c.throttle > 0.1) reasons.push(`CPU hit its limit in ${Math.round(c.throttle * 100)}% of 5-minute buckets — watch latency.`);
  if (confidence !== "high") reasons.push(`Only ${days} day${days === 1 ? "" : "s"} of history — confidence ${confidence}.`);
  if (reasons.length === 0) reasons.push("Requests already match measured usage within 10%.");

  return {
    id: `${w.cluster}/${w.namespace}/${w.name}/${c.name}`,
    cluster: w.cluster,
    namespace: w.namespace,
    workload: w.name,
    workloadKind: w.kind,
    container: c.name,
    replicas: w.replicas,
    cpu: { request: c.cpuReq, limit: c.cpuLim, p50: c.cpuP50, p95: c.cpuP95, p99: c.cpuP99, max: c.cpuMax, recommended: cpuRec },
    mem: {
      request: memReq,
      limit: c.memLimGiB * GIB,
      p50: c.memP50GiB * GIB,
      p99: memP99,
      max: memMax,
      recommended: memRec,
    },
    currentCostUsdMonth: current,
    recommendedCostUsdMonth: recommended,
    savingsUsdMonth: savings,
    samples: Math.round(days * 288 * w.replicas),
    window,
    confidence,
    direction,
    reason: reasons.join(" "),
    oomKills: c.oomKills,
    cpuThrottleRisk: c.throttle,
  };
}

function fmtCores(v: number): string {
  return v < 1 ? `${Math.round(v * 1000)}m` : `${Number(v.toFixed(2))} cores`;
}

export function demoRightsizing(opts: RightsizingOptions = {}): RightsizingView {
  const min = opts.minSavingsUsdMonth ?? 0;
  const recs: Rightsizing[] = [];
  for (const w of demoWorkloads()) {
    if (opts.clusterId && w.cluster !== opts.clusterId) continue;
    if (opts.namespace && w.namespace !== opts.namespace) continue;
    if (w.kind === "DaemonSet") continue;
    for (const c of w.containers) {
      const r = recommend(w, c, opts);
      // Upsizes (OOM / throttling) always surface — they're reliability, not savings.
      if (r.direction === "upsize" || (r.direction === "downsize" && r.savingsUsdMonth >= Math.max(min, 25))) recs.push(r);
    }
  }
  recs.sort((a, b) => b.savingsUsdMonth - a.savingsUsdMonth);
  return { recs, totalSavingsUsdMonth: recs.reduce((s, r) => s + Math.max(0, r.savingsUsdMonth), 0) };
}

// ─── allocation ─────────────────────────────────────────────────────────

let recoverableCache: Map<string, number> | null = null;
function recoverableFor(w: DemoWorkload): number {
  if (!recoverableCache) {
    recoverableCache = new Map();
    for (const r of demoRightsizing().recs) {
      const k = `${r.cluster}/${r.namespace}/${r.workload}`;
      recoverableCache.set(k, (recoverableCache.get(k) ?? 0) + Math.max(0, r.savingsUsdMonth));
    }
  }
  const base = recoverableCache.get(`${w.cluster}/${w.namespace}/${w.name}`) ?? 0;
  // GPU idle is recoverable too (not a request-sizing problem).
  const gpuIdle = w.gpuUsd > 0 && w.name === "model-server-a100" ? w.gpuUsd * 0.62 : 0;
  return base + gpuIdle;
}

/** Monthly-rate atoms for every workload, split across its nodes. */
export function demoAtoms(): AllocAtom[] {
  const out: AllocAtom[] = [];
  for (const w of demoWorkloads()) {
    const nodes = Math.max(1, Math.min(3, w.kind === "DaemonSet" ? 3 : w.replicas));
    const rec = recoverableFor(w);
    const cpuReq = w.containers.reduce((s, c) => s + c.cpuReq, 0) * w.replicas;
    const ramReq = w.containers.reduce((s, c) => s + c.memReqGiB, 0) * w.replicas * GIB;
    for (let i = 0; i < nodes; i++) {
      const f = 1 / nodes;
      out.push({
        cluster: w.cluster,
        namespace: w.namespace,
        workload: w.name,
        controllerKind: w.kind,
        team: w.team,
        costCenter: w.costCenter,
        node: nodeName(w.cluster, w.nodepool, i + (hash32(w.name) % 4)),
        nodepool: w.nodepool,
        zone: w.zone,
        labels: { ...w.labels, team: w.team },
        cpuCost: w.cpuUsd * f,
        ramCost: w.ramUsd * f,
        gpuCost: w.gpuUsd * f,
        networkCost: w.networkUsd * f,
        pvCost: w.pvUsd * f,
        recoverableCost: rec * f,
        cpuReqCores: cpuReq * f,
        cpuUseCores: cpuReq * w.cpuEff * f,
        ramReqBytes: ramReq * f,
        ramUseBytes: ramReq * w.ramEff * f,
        gpuHours: w.gpuCount * MONTH_H * f,
        logIngestGb: w.logGbDay * 30.4 * f,
      });
    }
  }
  return out;
}

/** Monthly idle (unallocated node capacity) per cluster. */
export function demoIdleMonthly(): Record<string, number> {
  const allocated = new Map<string, number>();
  for (const w of demoWorkloads()) allocated.set(w.cluster, (allocated.get(w.cluster) ?? 0) + totalUsd(w));
  const out: Record<string, number> = {};
  for (const cl of CLUSTERS) {
    const node = clusterMonthlyUsd(cl.id);
    out[cl.id] = Math.max(node * 0.05, node - (allocated.get(cl.id) ?? 0));
  }
  return out;
}

export type DemoAllocationRequest = {
  window: string;
  aggregate: Aggregate;
  filters?: Record<string, string>;
  includeIdle?: boolean;
  shareIdle?: "" | "weighted" | "even";
  sharedNamespaces?: string[];
  clusterId?: string;
};

export function demoAllocation(req: DemoAllocationRequest, now = Date.now()): AllocationView {
  const w: CostWindow = isCostWindow(req.window) ? req.window : "7d";
  const { startMs, endMs } = costWindowRange(w, now);
  const scale = Math.max(0, (endMs - startMs) / HOUR) / MONTH_H;
  const scaleAtom = (a: AllocAtom): AllocAtom => ({
    ...a,
    cpuCost: a.cpuCost * scale,
    ramCost: a.ramCost * scale,
    gpuCost: a.gpuCost * scale,
    networkCost: a.networkCost * scale,
    pvCost: a.pvCost * scale,
    recoverableCost: a.recoverableCost * scale,
    gpuHours: a.gpuHours * scale,
    logIngestGb: a.logIngestGb * scale,
  });
  let atoms = demoAtoms().map(scaleAtom);
  const idleMonthly = demoIdleMonthly();
  const idle: Record<string, number> = {};
  for (const [k, v] of Object.entries(idleMonthly)) idle[k] = v * scale;
  if (req.clusterId) {
    atoms = atoms.filter((a) => a.cluster === req.clusterId);
    for (const k of Object.keys(idle)) if (k !== req.clusterId) delete idle[k];
  }
  const rows = allocate(atoms, idle, {
    aggregate: [req.aggregate],
    filters: req.filters,
    includeIdle: req.includeIdle,
    shareIdle: req.shareIdle,
    sharedNamespaces: req.sharedNamespaces,
  });
  return { rows, totals: sumRows(rows), startMs, endMs };
}

// ─── cost time series ───────────────────────────────────────────────────

export type DemoSeriesRequest = {
  window?: string;
  groupBy?: string;
  filters?: Record<string, string>;
  top?: number;
  clusterId?: string;
};

function groupKey(w: DemoWorkload, groupBy: string): string {
  switch (groupBy) {
    case "namespace":
      return w.namespace;
    case "team":
      return w.team;
    case "cluster":
      return w.cluster;
    case "nodepool":
      return w.nodepool;
    case "workload":
      return `${w.namespace}/${w.name}`;
    default:
      return "total";
  }
}

function matchesFilters(w: DemoWorkload, filters: Record<string, string>): boolean {
  for (const [k, v] of Object.entries(filters)) {
    if (!v) continue;
    const val =
      k === "cluster" ? w.cluster : k === "namespace" ? w.namespace : k === "workload" ? w.name : k === "team" ? w.team : k === "nodepool" ? w.nodepool : undefined;
    if (val !== undefined && val !== v) return false;
  }
  return true;
}

/** Spend multiplier at time t for a workload: weekly cycle, trend, noise. */
function shapeAt(w: DemoWorkload, t: number, now: number, stepMs: number): number {
  const day = new Date(t).getUTCDay();
  const weekend = day === 0 || day === 6;
  const hour = new Date(t).getUTCHours();
  let f = 1;
  if (w.team === "commerce" || w.namespace === "edge") f *= weekend ? 0.86 : 1.04;
  if (w.namespace === "data" && stepMs < DAY) f *= hour >= 1 && hour <= 5 ? 2.2 : 0.7;
  if (w.namespace === "dev") f *= weekend ? 0.55 : 1.1;
  const daysAgo = (now - t) / DAY;
  f *= Math.max(0.2, 1 - w.trend * daysAgo);
  f *= 0.94 + unit("cost", w.cluster, w.namespace, w.name, Math.floor(t / stepMs)) * 0.12;
  // The ml-inference spike the anomaly card talks about: the last 4 days.
  if (w.name === "model-server-a100" && daysAgo < 4) f *= 1.34;
  return f;
}

export function demoCostSeries(req: DemoSeriesRequest, now = Date.now()): CostSeriesView {
  const w: CostWindow = isCostWindow(req.window ?? "") ? (req.window as CostWindow) : "30d";
  const { startMs, endMs } = costWindowRange(w, now);
  const stepMs = endMs - startMs <= 2 * DAY ? HOUR : DAY;
  const first = Math.floor(startMs / stepMs) * stepMs;
  const times: number[] = [];
  for (let t = first; t < endMs; t += stepMs) times.push(t);

  const filters: Record<string, string> = { ...(req.filters ?? {}), ...(req.clusterId ? { cluster: req.clusterId } : {}) };
  const groupBy = req.groupBy ?? "";
  const groups = new Map<string, number[]>();
  const idleMonthly = demoIdleMonthly();
  for (const wl of demoWorkloads()) {
    if (!matchesFilters(wl, filters)) continue;
    const key = groupKey(wl, groupBy);
    let vals = groups.get(key);
    if (!vals) {
      vals = new Array(times.length).fill(0);
      groups.set(key, vals);
    }
    const perStep = (totalUsd(wl) * (stepMs / HOUR)) / MONTH_H;
    for (let i = 0; i < times.length; i++) vals[i] += perStep * shapeAt(wl, times[i], now, stepMs);
  }
  // Idle capacity only shows up in fleet-wide / per-cluster totals.
  if (!filters.namespace && !filters.workload && !filters.team && (groupBy === "" || groupBy === "cluster")) {
    for (const [cluster, monthly] of Object.entries(idleMonthly)) {
      if (filters.cluster && filters.cluster !== cluster) continue;
      const key = groupBy === "cluster" ? cluster : "total";
      const vals = groups.get(key) ?? new Array(times.length).fill(0);
      const perStep = (monthly * (stepMs / HOUR)) / MONTH_H;
      for (let i = 0; i < times.length; i++) vals[i] += perStep;
      groups.set(key, vals);
    }
  }

  const sum = (v: number[]) => v.reduce((a, b) => a + b, 0);
  let entries = [...groups.entries()].sort((a, b) => sum(b[1]) - sum(a[1]));
  const top = req.top && req.top > 0 ? req.top : 8;
  if (entries.length > top) {
    const rest = entries.slice(top);
    const other = new Array(times.length).fill(0);
    for (const [, v] of rest) v.forEach((x, i) => (other[i] += x));
    entries = [...entries.slice(0, top), ["other", other]];
  }
  const series = entries.map(([key, values]) => ({ key, label: key, values }));
  const totalUsdAll = series.reduce((s, x) => s + sum(x.values), 0);

  // Forecast: month-to-date + trailing-7d daily average × days left.
  const d = new Date(now);
  const monthStart = Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), 1);
  const monthEnd = Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1);
  const allDaily = demoDailyTotal(filters, now, 7);
  const avg7 = allDaily.reduce((a, b) => a + b, 0) / Math.max(1, allDaily.length);
  const mtdDays = (now - monthStart) / DAY;
  const forecast = avg7 * mtdDays + avg7 * ((monthEnd - now) / DAY);

  return { times, stepMs, series, totalUsd: totalUsdAll, forecastMonthUsd: forecast };
}

function demoDailyTotal(filters: Record<string, string>, now: number, days: number): number[] {
  const out: number[] = [];
  for (let i = days; i >= 1; i--) {
    const t = Math.floor((now - i * DAY) / DAY) * DAY;
    let s = 0;
    for (const wl of demoWorkloads()) {
      if (!matchesFilters(wl, filters)) continue;
      s += (totalUsd(wl) * 24 * shapeAt(wl, t, now, DAY)) / MONTH_H;
    }
    out.push(s);
  }
  return out;
}

// ─── efficiency ─────────────────────────────────────────────────────────

/** score = 100 × cost-weighted (cpu, ram) efficiency × (1 − idle share). */
export function efficiencyScore(cpuEff: number, ramEff: number, cpuCost: number, ramCost: number, idleShare: number): number {
  const blend = cpuCost + ramCost > 0 ? (Math.min(1, cpuEff) * cpuCost + Math.min(1, ramEff) * ramCost) / (cpuCost + ramCost) : 0;
  return Math.max(0, Math.min(100, 100 * blend * (1 - Math.max(0, Math.min(1, idleShare)))));
}

export function demoEfficiency(opts: { clusterId?: string } = {}, now = Date.now()): EfficiencyView {
  const byCluster = demoAllocation({ window: "7d", aggregate: "cluster", includeIdle: true, clusterId: opts.clusterId }, now);
  const byNs = demoAllocation({ window: "7d", aggregate: "namespace", clusterId: opts.clusterId }, now);
  const scale = MONTH_H / (7 * 24);
  const idleByCluster = new Map<string, number>();
  for (const r of byCluster.rows) if (r.isIdle) idleByCluster.set(r.name.split("/")[0], r.idleCost);
  const alloc = byCluster.rows.filter((r) => !r.isIdle);
  const clusters: EfficiencyBreakdown[] = alloc.map((r) => {
    const idle = idleByCluster.get(r.name) ?? 0;
    const total = r.totalCost + idle;
    return {
      name: r.name,
      cpuEfficiency: r.cpuEfficiency,
      ramEfficiency: r.ramEfficiency,
      idleCostUsdMonth: idle * scale,
      totalCostUsdMonth: total * scale,
      score: efficiencyScore(r.cpuEfficiency, r.ramEfficiency, r.cpuCost, r.ramCost, total > 0 ? idle / total : 0),
    };
  });
  const namespaces: EfficiencyBreakdown[] = byNs.rows
    .filter((r) => !r.isIdle)
    .slice(0, 25)
    .map((r) => ({
      name: r.name,
      cpuEfficiency: r.cpuEfficiency,
      ramEfficiency: r.ramEfficiency,
      idleCostUsdMonth: 0,
      totalCostUsdMonth: r.totalCost * scale,
      score: efficiencyScore(r.cpuEfficiency, r.ramEfficiency, r.cpuCost, r.ramCost, 0),
    }));
  const t = sumRows(alloc);
  const idleTotal = [...idleByCluster.values()].reduce((a, b) => a + b, 0);
  const all = t.totalCost + idleTotal;
  return {
    score: efficiencyScore(t.cpuEfficiency, t.ramEfficiency, t.cpuCost, t.ramCost, all > 0 ? idleTotal / all : 0),
    cpuEfficiency: t.cpuEfficiency,
    ramEfficiency: t.ramEfficiency,
    idleCostUsdMonth: idleTotal * scale,
    recoverableUsdMonth: t.recoverableCost * scale,
    clusters: clusters.sort((a, b) => b.totalCostUsdMonth - a.totalCostUsdMonth),
    namespaces,
  };
}

