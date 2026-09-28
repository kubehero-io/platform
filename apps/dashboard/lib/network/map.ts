// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// NetworkService JSON → view models. Edges pointing at unknown nodes are
// dropped (a truncated map must still render), duplicate edges merged.

import { arr, bool, num, str } from "@/lib/api/wire";
import type {
  GetServiceMapResponseJson,
  ListNetworkCostsResponseJson,
  MapEdge,
  MapNode,
  NetworkCost,
  ServiceMap,
} from "./types";

export function toServiceMap(res: GetServiceMapResponseJson): ServiceMap {
  const nodes: MapNode[] = [];
  const seen = new Set<string>();
  for (const n of arr(res.nodes)) {
    const id = str(n.id);
    if (!id || seen.has(id)) continue;
    seen.add(id);
    nodes.push({
      id,
      name: str(n.name) || id.split("/").pop() || id,
      namespace: str(n.namespace),
      kind: str(n.kind, "workload"),
      zone: str(n.zone),
      bytesIn: num(n.bytesIn),
      bytesOut: num(n.bytesOut),
      costUsdMonth: num(n.costUsdMonth),
    });
  }
  const merged = new Map<string, MapEdge>();
  for (const e of arr(res.edges)) {
    const source = str(e.source);
    const target = str(e.target);
    if (!seen.has(source) || !seen.has(target) || source === target) continue;
    const port = num(e.port);
    const protocol = str(e.protocol, "tcp");
    const id = `${source}→${target}:${port}/${protocol}`;
    const prev = merged.get(id);
    const next: MapEdge = {
      id,
      source,
      target,
      port,
      protocol,
      bytes: num(e.bytes) + (prev?.bytes ?? 0),
      bytesPerSec: num(e.bytesPerSec) + (prev?.bytesPerSec ?? 0),
      crossZone: bool(e.crossZone) || !!prev?.crossZone,
      egress: bool(e.egress) || !!prev?.egress,
      costUsdMonth: num(e.costUsdMonth) + (prev?.costUsdMonth ?? 0),
      retransmits: num(e.retransmits) + (prev?.retransmits ?? 0),
    };
    merged.set(id, next);
  }
  const edges = [...merged.values()];
  return {
    nodes,
    edges,
    totalCostUsdMonth: num(res.totalCostUsdMonth) || edges.reduce((s, e) => s + e.costUsdMonth, 0),
    crossZoneGb: num(res.crossZoneGb) || edges.filter((e) => e.crossZone).reduce((s, e) => s + e.bytes / 1e9, 0),
    egressGb: num(res.egressGb) || edges.filter((e) => e.egress).reduce((s, e) => s + e.bytes / 1e9, 0),
  };
}

export function toNetworkCosts(res: ListNetworkCostsResponseJson): { costs: NetworkCost[]; totalUsdMonth: number } {
  const costs = arr(res.costs).map((c) => ({
    namespace: str(c.namespace),
    workload: str(c.workload),
    egressGb: num(c.egressGb),
    crossZoneGb: num(c.crossZoneGb),
    egressUsdMonth: num(c.egressUsdMonth),
    crossZoneUsdMonth: num(c.crossZoneUsdMonth),
    totalUsdMonth: num(c.totalUsdMonth) || num(c.egressUsdMonth) + num(c.crossZoneUsdMonth),
    topDestination: str(c.topDestination),
  }));
  return { costs, totalUsdMonth: num(res.totalUsdMonth) || costs.reduce((s, c) => s + c.totalUsdMonth, 0) };
}
