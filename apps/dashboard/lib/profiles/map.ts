// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// ProfilesService JSON → view models. int64 frame values arrive as
// strings; a flamegraph with a broken parent index is repaired (reparent
// to root) rather than crashing the canvas.

import { arr, int64, num, str } from "@/lib/api/wire";
import type { FlameNode } from "@/lib/flame/tree";
import type {
  Flamegraph,
  GetFlamegraphResponseJson,
  GetTopFunctionsResponseJson,
  ProfileTarget,
  ProfileTargetJson,
  TopFunction,
} from "./types";

export function toProfileTarget(t: ProfileTargetJson): ProfileTarget {
  return {
    service: str(t.service) || str(t.workload),
    namespace: str(t.namespace),
    workload: str(t.workload) || str(t.service),
    types: arr(t.types).filter((x): x is string => typeof x === "string"),
    origin: str(t.origin, "ebpf"),
    lastSeenMs: int64(t.lastSeenUnixMs),
    cpuCoresAvg: num(t.cpuCoresAvg),
    costUsdMonth: num(t.costUsdMonth),
  };
}

export function toFlamegraph(res: GetFlamegraphResponseJson): Flamegraph {
  const raw = arr(res.nodes);
  const nodes: FlameNode[] = raw.map((n, i) => {
    const parent = typeof n.parent === "number" && n.parent >= 0 && n.parent < i ? n.parent : i === 0 ? -1 : 0;
    return {
      name: str(n.name, i === 0 ? "total" : "[unknown]"),
      parent,
      depth: 0,
      self: int64(n.self),
      total: int64(n.total),
      baselineSelf: int64(n.baselineSelf),
      baselineTotal: int64(n.baselineTotal),
    };
  });
  // Recompute depth from parents (the wire value is advisory).
  for (let i = 1; i < nodes.length; i++) nodes[i].depth = nodes[nodes[i].parent].depth + 1;
  if (nodes.length > 0) nodes[0].parent = -1;
  const total = int64(res.total) || (nodes[0]?.total ?? 0);
  if (nodes.length > 0 && nodes[0].total === 0) nodes[0].total = total;
  return {
    nodes,
    total,
    unit: str(res.unit, "nanoseconds"),
    type: str(res.type, "cpu"),
    baselineTotal: int64(res.baselineTotal),
    samples: int64(res.samples),
    costUsdMonth: num(res.costUsdMonth),
  };
}

export function toTopFunctions(res: GetTopFunctionsResponseJson): TopFunction[] {
  return arr(res.functions).map((f) => ({
    name: str(f.name, "[unknown]"),
    self: int64(f.self),
    total: int64(f.total),
    selfPct: num(f.selfPct),
    totalPct: num(f.totalPct),
    selfCostUsdMonth: num(f.selfCostUsdMonth),
  }));
}

/** Format a profile value by its unit. */
export function formatProfileValue(v: number, unit: string): string {
  if (unit === "nanoseconds") {
    const s = v / 1e9;
    if (s >= 3600) return `${(s / 3600).toFixed(1)} h`;
    if (s >= 60) return `${(s / 60).toFixed(1)} min`;
    if (s >= 1) return `${s.toFixed(s >= 10 ? 0 : 2)} s`;
    return `${(v / 1e6).toFixed(1)} ms`;
  }
  if (unit === "bytes") {
    const u = ["B", "KiB", "MiB", "GiB", "TiB"];
    let x = v;
    let i = 0;
    while (x >= 1024 && i < u.length - 1) {
      x /= 1024;
      i++;
    }
    return `${x.toFixed(i === 0 ? 0 : 1)} ${u[i]}`;
  }
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`;
  if (v >= 1e3) return `${(v / 1e3).toFixed(1)}k`;
  return String(Math.round(v));
}
