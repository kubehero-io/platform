// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Allocation math, OpenCost semantics — used by the demo engine (so demo
// mode honours every control on /allocation) and by the page for
// drill-down, shares and CSV export. Pure and unit-tested.
//
// Order of operations (matches the control plane's engine):
//   1. shared namespaces: their cost is removed and redistributed to the
//      other rows of the SAME cluster, weighted by each row's own cost;
//   2. idle: per-cluster unallocated node cost is either kept as an
//      "__idle__" row (includeIdle), spread "weighted" by cost or "even"
//      per row, or dropped;
//   3. filters (exact match) — after sharing, so a filtered view still
//      carries its fair share of kube-system and idle;
//   4. aggregation by one or more dimensions (composite key "a/b"),
//      summing costs and request/usage so efficiency = usage ÷ request.

import { IDLE_NAME, sumRows } from "./map";
import type { Aggregate, AllocationRow } from "./types";

export type AllocAtom = {
  cluster: string;
  namespace: string;
  workload: string;
  controllerKind: string;
  team: string;
  costCenter: string;
  node: string;
  nodepool: string;
  zone: string;
  labels: Record<string, string>;
  cpuCost: number;
  ramCost: number;
  gpuCost: number;
  networkCost: number;
  pvCost: number;
  recoverableCost: number;
  cpuReqCores: number;
  cpuUseCores: number;
  ramReqBytes: number;
  ramUseBytes: number;
  gpuHours: number;
  logIngestGb: number;
};

export type AllocOptions = {
  aggregate: Aggregate[];
  filters?: Record<string, string>;
  includeIdle?: boolean;
  shareIdle?: "" | "weighted" | "even";
  sharedNamespaces?: string[];
};

type Working = AllocAtom & { sharedCost: number; idleCost: number };

export const UNALLOCATED = "__unallocated__";

function atomCost(a: { cpuCost: number; ramCost: number; gpuCost: number; networkCost: number; pvCost: number }): number {
  return a.cpuCost + a.ramCost + a.gpuCost + a.networkCost + a.pvCost;
}

/** Value of a dimension for an atom ("label:app" reads the labels map). */
export function dimValue(a: AllocAtom, dim: string): string {
  switch (dim) {
    case "cluster":
      return a.cluster;
    case "namespace":
      return a.namespace;
    case "workload":
      return a.workload;
    case "controller":
      return `${a.controllerKind.toLowerCase()}:${a.workload}`;
    case "team":
      return a.team || UNALLOCATED;
    case "cost_center":
      return a.costCenter || UNALLOCATED;
    case "node":
      return a.node;
    case "nodepool":
      return a.nodepool;
    case "zone":
      return a.zone;
    default:
      if (dim.startsWith("label:")) return a.labels[dim.slice(6)] ?? UNALLOCATED;
      return UNALLOCATED;
  }
}

export function allocate(atoms: AllocAtom[], idleByCluster: Record<string, number>, opts: AllocOptions): AllocationRow[] {
  const shared = new Set(opts.sharedNamespaces ?? []);
  let work: Working[] = atoms.map((a) => ({ ...a, sharedCost: 0, idleCost: 0 }));

  // 1. shared namespaces → redistribute within the cluster by cost weight.
  if (shared.size > 0) {
    const pool = new Map<string, number>();
    for (const a of work) if (shared.has(a.namespace)) pool.set(a.cluster, (pool.get(a.cluster) ?? 0) + atomCost(a));
    work = work.filter((a) => !shared.has(a.namespace));
    const weight = new Map<string, number>();
    for (const a of work) weight.set(a.cluster, (weight.get(a.cluster) ?? 0) + atomCost(a));
    for (const a of work) {
      const w = weight.get(a.cluster) ?? 0;
      if (w > 0) a.sharedCost = ((pool.get(a.cluster) ?? 0) * atomCost(a)) / w;
    }
  }

  // 2. idle.
  const idleRows: Working[] = [];
  const mode = opts.shareIdle ?? "";
  if (mode === "weighted" || mode === "even") {
    const weight = new Map<string, number>();
    const count = new Map<string, number>();
    for (const a of work) {
      weight.set(a.cluster, (weight.get(a.cluster) ?? 0) + atomCost(a) + a.sharedCost);
      count.set(a.cluster, (count.get(a.cluster) ?? 0) + 1);
    }
    for (const a of work) {
      const idle = idleByCluster[a.cluster] ?? 0;
      if (mode === "weighted") {
        const w = weight.get(a.cluster) ?? 0;
        if (w > 0) a.idleCost = (idle * (atomCost(a) + a.sharedCost)) / w;
      } else {
        const n = count.get(a.cluster) ?? 0;
        if (n > 0) a.idleCost = idle / n;
      }
    }
  } else if (opts.includeIdle) {
    for (const [cluster, idle] of Object.entries(idleByCluster)) {
      if (idle <= 0) continue;
      idleRows.push({
        cluster,
        namespace: IDLE_NAME,
        workload: IDLE_NAME,
        controllerKind: "",
        team: "",
        costCenter: "",
        node: "",
        nodepool: "",
        zone: "",
        labels: {},
        cpuCost: 0,
        ramCost: 0,
        gpuCost: 0,
        networkCost: 0,
        pvCost: 0,
        recoverableCost: 0,
        cpuReqCores: 0,
        cpuUseCores: 0,
        ramReqBytes: 0,
        ramUseBytes: 0,
        gpuHours: 0,
        logIngestGb: 0,
        sharedCost: 0,
        idleCost: idle,
      });
    }
  }

  // 3. filters (idle rows only survive a cluster filter).
  const filters = Object.entries(opts.filters ?? {}).filter(([, v]) => v !== "");
  if (filters.length > 0) {
    work = work.filter((a) => filters.every(([k, v]) => dimValue(a, k) === v));
  }
  const keptIdle = idleRows.filter((r) => filters.every(([k, v]) => k === "cluster" && r.cluster === v));

  // 4. aggregate.
  const dims = opts.aggregate.length > 0 ? opts.aggregate : ["namespace"];
  const groups = new Map<string, { rows: Working[]; props: Record<string, string> }>();
  for (const a of work) {
    const key = dims.map((d) => dimValue(a, d)).join("/");
    let g = groups.get(key);
    if (!g) {
      g = { rows: [], props: commonProps(a) };
      groups.set(key, g);
    } else {
      g.props = intersectProps(g.props, commonProps(a));
    }
    g.rows.push(a);
  }

  const out: AllocationRow[] = [];
  for (const [name, g] of groups) out.push(toRow(name, g.rows, g.props));

  if (keptIdle.length > 0) {
    if (dims.includes("cluster") && dims.length === 1) {
      for (const r of keptIdle) out.push(toRow(`${r.cluster}/${IDLE_NAME}`, [r], { cluster: r.cluster }, true));
    } else {
      out.push(toRow(IDLE_NAME, keptIdle, keptIdle.length === 1 ? { cluster: keptIdle[0].cluster } : {}, true));
    }
  }
  return out.sort((a, b) => b.totalCost - a.totalCost);
}

function commonProps(a: AllocAtom): Record<string, string> {
  return {
    cluster: a.cluster,
    namespace: a.namespace,
    workload: a.workload,
    controllerKind: a.controllerKind,
    team: a.team,
    node: a.node,
    nodepool: a.nodepool,
    zone: a.zone,
  };
}

/** Keep only properties every member agrees on (like OpenCost). */
function intersectProps(a: Record<string, string>, b: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(a)) if (b[k] === v) out[k] = v;
  return out;
}

function toRow(name: string, rows: Working[], props: Record<string, string>, idle = false): AllocationRow {
  let cpuCost = 0;
  let ramCost = 0;
  let gpuCost = 0;
  let networkCost = 0;
  let pvCost = 0;
  let sharedCost = 0;
  let idleCost = 0;
  let recoverable = 0;
  let cpuReq = 0;
  let cpuUse = 0;
  let ramReq = 0;
  let ramUse = 0;
  let gpuHours = 0;
  let logGb = 0;
  for (const r of rows) {
    cpuCost += r.cpuCost;
    ramCost += r.ramCost;
    gpuCost += r.gpuCost;
    networkCost += r.networkCost;
    pvCost += r.pvCost;
    sharedCost += r.sharedCost;
    idleCost += r.idleCost;
    recoverable += r.recoverableCost;
    cpuReq += r.cpuReqCores;
    cpuUse += r.cpuUseCores;
    ramReq += r.ramReqBytes;
    ramUse += r.ramUseBytes;
    gpuHours += r.gpuHours;
    logGb += r.logIngestGb;
  }
  const cpuEff = cpuReq > 0 ? cpuUse / cpuReq : 0;
  const ramEff = ramReq > 0 ? ramUse / ramReq : 0;
  const blend = cpuCost + ramCost > 0 ? (cpuEff * cpuCost + ramEff * ramCost) / (cpuCost + ramCost) : 0;
  return {
    name,
    properties: props,
    cpuCost,
    ramCost,
    gpuCost,
    networkCost,
    pvCost,
    sharedCost,
    idleCost,
    totalCost: cpuCost + ramCost + gpuCost + networkCost + pvCost + sharedCost + idleCost,
    cpuEfficiency: cpuEff,
    ramEfficiency: ramEff,
    totalEfficiency: blend,
    recoverableCost: recoverable,
    cpuCoreRequestAverage: cpuReq,
    cpuCoreUsageAverage: cpuUse,
    ramByteRequestAverage: ramReq,
    ramByteUsageAverage: ramUse,
    gpuHours,
    logIngestGb: logGb,
    isIdle: idle,
  };
}

export { sumRows };

// ─── drill-down ─────────────────────────────────────────────────────────

export type Drill =
  | { kind: "aggregate"; aggregate: Aggregate; filters: Record<string, string> }
  | { kind: "workload"; cluster: string; namespace: string; workload: string }
  | null;

const NEXT_DIM: Record<string, Aggregate> = {
  cluster: "namespace",
  namespace: "workload",
  team: "namespace",
  cost_center: "team",
  nodepool: "node",
  node: "namespace",
  zone: "namespace",
};

/** Where clicking a row goes, given the current aggregate + filters. */
export function drillTarget(aggregate: Aggregate, row: AllocationRow, filters: Record<string, string>): Drill {
  if (row.isIdle || row.name === UNALLOCATED) return null;
  if (aggregate === "workload" || aggregate === "controller") {
    const cluster = row.properties.cluster || filters.cluster;
    const namespace = row.properties.namespace || filters.namespace;
    const workload = row.properties.workload || row.name.split(":").pop() || row.name;
    return cluster && namespace ? { kind: "workload", cluster, namespace, workload } : null;
  }
  const next = aggregate.startsWith("label:") ? "namespace" : NEXT_DIM[aggregate];
  if (!next) return null;
  return { kind: "aggregate", aggregate: next, filters: { ...filters, [aggregate]: row.name } };
}

// ─── CSV ────────────────────────────────────────────────────────────────

/** RFC 4180 field, with spreadsheet formula injection neutralised. */
export function csvField(v: string | number): string {
  let s = typeof v === "number" ? (Number.isFinite(v) ? String(Math.round(v * 10000) / 10000) : "") : v;
  if (typeof v === "string" && /^[=+\-@\t\r]/.test(s)) s = `'${s}`;
  return /[",\r\n]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

export const CSV_COLUMNS = [
  "name", "cluster", "namespace", "cpu_cost", "ram_cost", "gpu_cost", "network_cost", "pv_cost", "shared_cost",
  "idle_cost", "total_cost", "cpu_efficiency", "ram_efficiency", "total_efficiency", "recoverable_cost",
  "cpu_request_cores", "cpu_usage_cores", "ram_request_bytes", "ram_usage_bytes", "gpu_hours", "log_ingest_gb",
] as const;

/** `source` ("live" | "demo") is appended as a column so a demo export can never pass for billing data. */
export function allocationCsv(rows: AllocationRow[], source?: string): string {
  const lines = [[...CSV_COLUMNS, ...(source ? ["source"] : [])].join(",")];
  for (const r of rows) {
    lines.push(
      [
        r.name, r.properties.cluster ?? "", r.properties.namespace ?? "", r.cpuCost, r.ramCost, r.gpuCost,
        r.networkCost, r.pvCost, r.sharedCost, r.idleCost, r.totalCost, r.cpuEfficiency, r.ramEfficiency,
        r.totalEfficiency, r.recoverableCost, r.cpuCoreRequestAverage, r.cpuCoreUsageAverage,
        r.ramByteRequestAverage, r.ramByteUsageAverage, r.gpuHours, r.logIngestGb,
        ...(source ? [source] : []),
      ]
        .map(csvField)
        .join(","),
    );
  }
  return lines.join("\r\n") + "\r\n";
}
