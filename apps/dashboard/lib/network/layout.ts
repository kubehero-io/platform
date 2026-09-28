// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Service-map geometry: a deterministic d3-force layout (same map → same
// picture on every render, server or client), zone-aware — each
// availability zone pulls its workloads into its own column so
// cross-zone traffic reads as long horizontal edges, and external
// destinations sit on the right edge. Plus the visual encodings:
// node radius ∝ √bytes, edge width ∝ √bytes/s, edge colour by cost class.
//
// Pure (d3-force is DOM-free) and unit-tested.

import { forceCollide, forceLink, forceManyBody, forceSimulation, forceX, forceY, type SimulationNodeDatum } from "d3-force";
import type { MapEdge, MapNode } from "./types";

export type Placed = MapNode & { x: number; y: number; r: number };

export type EdgeClass = "cross-zone" | "egress" | "internal";

export const EDGE_COLOR: Record<EdgeClass, string> = {
  "cross-zone": "var(--color-warn)",
  egress: "var(--color-accent)",
  internal: "var(--color-fg-faint)",
};

export function edgeClass(e: Pick<MapEdge, "crossZone" | "egress">): EdgeClass {
  return e.egress ? "egress" : e.crossZone ? "cross-zone" : "internal";
}

/** Retransmits per packet sent (≈1460-byte segments) above 0.5% → dashed. */
export function retransmitHeavy(e: Pick<MapEdge, "retransmits" | "bytes">): boolean {
  const packets = Math.max(1, e.bytes / 1460);
  return e.retransmits / packets > 0.005;
}

export function nodeRadius(bytes: number, maxBytes: number, min = 6, max = 26): number {
  if (!(maxBytes > 0) || !(bytes > 0)) return min;
  return min + (max - min) * Math.sqrt(Math.min(1, bytes / maxBytes));
}

export function edgeWidth(bps: number, maxBps: number, min = 1, max = 8): number {
  if (!(maxBps > 0) || !(bps > 0)) return min;
  return min + (max - min) * Math.sqrt(Math.min(1, bps / maxBps));
}

function hash01(s: string): number {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return (h >>> 0) / 4294967296;
}

/** Column centre per zone (sorted), externals at the far right. */
export function zoneColumns(nodes: MapNode[], width: number): Map<string, number> {
  const zones = [...new Set(nodes.filter((n) => n.kind !== "external" && n.zone).map((n) => n.zone))].sort();
  const cols = new Map<string, number>();
  const usable = width * (nodes.some((n) => n.kind === "external") ? 0.78 : 0.92);
  zones.forEach((z, i) => cols.set(z, width * 0.06 + (usable * (i + 0.5)) / Math.max(1, zones.length)));
  return cols;
}

type SimNode = SimulationNodeDatum & { id: string; r: number; tx: number; external: boolean };

export function layoutServiceMap(
  nodes: MapNode[],
  edges: MapEdge[],
  opts: { width: number; height: number; iterations?: number },
): Placed[] {
  const { width, height } = opts;
  const sorted = [...nodes].sort((a, b) => a.id.localeCompare(b.id));
  const maxBytes = Math.max(1, ...sorted.map((n) => n.bytesIn + n.bytesOut));
  const cols = zoneColumns(sorted, width);
  const sim: SimNode[] = sorted.map((n) => {
    const r = nodeRadius(n.bytesIn + n.bytesOut, maxBytes);
    const external = n.kind === "external";
    const tx = external ? width * 0.93 : (cols.get(n.zone) ?? width * 0.45);
    return {
      id: n.id,
      r,
      tx,
      external,
      x: tx + (hash01(n.id) - 0.5) * 60,
      y: height * (0.1 + 0.8 * hash01(`${n.id}/y`)),
      // Externals are pinned to the right edge; only their height floats.
      fx: external ? tx : undefined,
    };
  });
  const ids = new Set(sim.map((n) => n.id));
  const links = edges
    .filter((e) => ids.has(e.source) && ids.has(e.target))
    .map((e) => ({ source: e.source, target: e.target, w: e.bytesPerSec }));
  const maxW = Math.max(1, ...links.map((l) => l.w));

  const simulation = forceSimulation<SimNode>(sim)
    .randomSource(() => 0.5) // deterministic jiggle
    .force(
      "link",
      forceLink<SimNode, { source: string; target: string; w: number }>(links)
        .id((d) => d.id)
        .distance(90)
        .strength((l) => 0.05 + 0.25 * Math.sqrt(l.w / maxW)),
    )
    .force("charge", forceManyBody<SimNode>().strength(-420).distanceMax(width / 2))
    .force("x", forceX<SimNode>((d) => d.tx).strength((d) => (d.external ? 1 : 0.6)))
    .force("y", forceY<SimNode>(height / 2).strength(0.025))
    .force("collide", forceCollide<SimNode>((d) => d.r + 22).iterations(2))
    .stop();
  for (let i = 0; i < (opts.iterations ?? 300); i++) simulation.tick();

  const byId = new Map(sim.map((s) => [s.id, s]));
  return sorted.map((n) => {
    const s = byId.get(n.id)!;
    const pad = s.r + 18;
    return {
      ...n,
      r: s.r,
      x: Math.max(pad, Math.min(width - pad, s.x ?? width / 2)),
      y: Math.max(pad, Math.min(height - pad, s.y ?? height / 2)),
    };
  });
}

/**
 * Quadratic edge from the rim of `s` to the rim of `t`. Pairs with traffic
 * both ways bend to opposite sides (`bend` ±) so they don't overlap.
 */
export function edgePath(
  s: { x: number; y: number; r: number },
  t: { x: number; y: number; r: number },
  bend = 0,
): { d: string; mx: number; my: number } {
  const dx = t.x - s.x;
  const dy = t.y - s.y;
  const len = Math.hypot(dx, dy) || 1;
  const ux = dx / len;
  const uy = dy / len;
  // Control point offset perpendicular to the chord.
  const off = bend * Math.min(60, len * 0.25);
  const cx = (s.x + t.x) / 2 - uy * off;
  const cy = (s.y + t.y) / 2 + ux * off;
  const sx = s.x + ux * s.r;
  const sy = s.y + uy * s.r;
  const tx = t.x - ux * (t.r + 4); // leave room for the arrowhead
  const ty = t.y - uy * (t.r + 4);
  const mx = 0.25 * sx + 0.5 * cx + 0.25 * tx;
  const my = 0.25 * sy + 0.5 * cy + 0.25 * ty;
  return { d: `M${sx.toFixed(1)},${sy.toFixed(1)} Q${cx.toFixed(1)},${cy.toFixed(1)} ${tx.toFixed(1)},${ty.toFixed(1)}`, mx, my };
}

/** Edges touching a node, split by direction, sorted by cost then bytes. */
export function edgesOf(nodeId: string, edges: MapEdge[]): { inbound: MapEdge[]; outbound: MapEdge[] } {
  const by = (a: MapEdge, b: MapEdge) => b.costUsdMonth - a.costUsdMonth || b.bytes - a.bytes;
  return {
    inbound: edges.filter((e) => e.target === nodeId).sort(by),
    outbound: edges.filter((e) => e.source === nodeId).sort(by),
  };
}

/** Keep the heaviest `max` nodes (by traffic); fold the rest into one "other" node. */
export function capNodes(nodes: MapNode[], edges: MapEdge[], max: number): { nodes: MapNode[]; edges: MapEdge[] } {
  if (nodes.length <= max) return { nodes, edges };
  const ranked = [...nodes].sort((a, b) => b.bytesIn + b.bytesOut - (a.bytesIn + a.bytesOut));
  const keep = new Set(ranked.slice(0, max - 1).map((n) => n.id));
  const rest = ranked.slice(max - 1);
  const other: MapNode = {
    id: "other:*",
    name: `other (${rest.length})`,
    namespace: "",
    kind: "service",
    zone: "",
    bytesIn: rest.reduce((s, n) => s + n.bytesIn, 0),
    bytesOut: rest.reduce((s, n) => s + n.bytesOut, 0),
    costUsdMonth: rest.reduce((s, n) => s + n.costUsdMonth, 0),
  };
  const map = (id: string) => (keep.has(id) ? id : other.id);
  const merged = new Map<string, MapEdge>();
  for (const e of edges) {
    const source = map(e.source);
    const target = map(e.target);
    if (source === target) continue;
    const id = `${source}→${target}:${e.port}/${e.protocol}`;
    const p = merged.get(id);
    merged.set(id, {
      ...e,
      id,
      source,
      target,
      bytes: e.bytes + (p?.bytes ?? 0),
      bytesPerSec: e.bytesPerSec + (p?.bytesPerSec ?? 0),
      costUsdMonth: e.costUsdMonth + (p?.costUsdMonth ?? 0),
      retransmits: e.retransmits + (p?.retransmits ?? 0),
      crossZone: e.crossZone || !!p?.crossZone,
      egress: e.egress || !!p?.egress,
    });
  }
  return { nodes: [...nodes.filter((n) => keep.has(n.id)), other], edges: [...merged.values()] };
}
