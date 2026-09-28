// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Service map: deterministic, zone-aware force layout (lib/network/
// layout.ts) drawn as SVG — ≤ 60 nodes by contract, so SVG stays crisp
// and every node is a focusable, labelled element. Wheel zooms around
// the cursor, drag pans; click (or ⏎) a node for its edges and costs.

import Link from "next/link";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Minus, Plus, Scan } from "lucide-react";
import { Drawer } from "@/components/ui/drawer";
import {
  EDGE_COLOR,
  edgeClass,
  edgePath,
  edgeWidth,
  edgesOf,
  layoutServiceMap,
  retransmitHeavy,
  zoneColumns,
} from "@/lib/network/layout";
import type { MapEdge, MapNode } from "@/lib/network/types";
import { formatBytes, formatGB, formatUsd } from "@/lib/chart/scale";
import { logqlSelector, logsHref, workloadHref } from "@/lib/url";

const W = 1200;
const H = 620;

type View = { k: number; x: number; y: number };

export function ServiceMap({ nodes, edges, cluster }: { nodes: MapNode[]; edges: MapEdge[]; cluster?: string }) {
  const placed = useMemo(() => layoutServiceMap(nodes, edges, { width: W, height: H }), [nodes, edges]);
  const byId = useMemo(() => new Map(placed.map((n) => [n.id, n])), [placed]);
  const cols = useMemo(() => zoneColumns(nodes, W), [nodes]);
  const maxBps = useMemo(() => Math.max(1, ...edges.map((e) => e.bytesPerSec)), [edges]);
  const pairs = useMemo(() => new Set(edges.map((e) => `${e.source}|${e.target}`)), [edges]);
  const [view, setView] = useState<View>({ k: 1, x: 0, y: 0 });
  const [selected, setSelected] = useState<string | null>(null);
  const [hoverEdge, setHoverEdge] = useState<{ e: MapEdge; x: number; y: number } | null>(null);
  const [hoverNode, setHoverNode] = useState<string | null>(null);
  const svgRef = useRef<SVGSVGElement>(null);
  const drag = useRef<{ x: number; y: number; vx: number; vy: number } | null>(null);
  const [dragging, setDragging] = useState(false);

  const toLocal = (clientX: number, clientY: number) => {
    const r = svgRef.current?.getBoundingClientRect();
    if (!r) return { x: 0, y: 0 };
    return { x: ((clientX - r.left) / r.width) * W, y: ((clientY - r.top) / r.height) * H };
  };

  const zoomAt = useCallback((factor: number, cx = W / 2, cy = H / 2) => {
    setView((v) => {
      const k = Math.max(0.4, Math.min(4, v.k * factor));
      const f = k / v.k;
      return { k, x: cx - (cx - v.x) * f, y: cy - (cy - v.y) * f };
    });
  }, []);

  // ⌘/ctrl + wheel (and trackpad pinch, which arrives as ctrl+wheel)
  // zooms around the cursor; a plain wheel keeps scrolling the page.
  // Needs a non-passive native listener to stop the browser's page zoom.
  useEffect(() => {
    const el = svgRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      const r = el.getBoundingClientRect();
      const x = ((e.clientX - r.left) / r.width) * W;
      const y = ((e.clientY - r.top) / r.height) * H;
      zoomAt(e.deltaY < 0 ? 1.1 : 1 / 1.1, x, y);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [zoomAt]);

  const selectedNode = selected ? byId.get(selected) : undefined;
  const sel = selected ? edgesOf(selected, edges) : null;
  const neighbours = useMemo(() => {
    const focus = hoverNode ?? selected;
    if (!focus) return null;
    const s = new Set<string>([focus]);
    for (const e of edges) {
      if (e.source === focus) s.add(e.target);
      if (e.target === focus) s.add(e.source);
    }
    return s;
  }, [hoverNode, selected, edges]);

  return (
    <div className="relative">
      <div className="absolute right-2 top-2 z-10 flex flex-col gap-px border border-[var(--color-line-bright)] bg-[var(--color-line)]">
        <MapButton label="zoom in" onClick={() => zoomAt(1.25)} icon={Plus} />
        <MapButton label="zoom out" onClick={() => zoomAt(1 / 1.25)} icon={Minus} />
        <MapButton label="fit" onClick={() => setView({ k: 1, x: 0, y: 0 })} icon={Scan} />
      </div>

      <svg
        ref={svgRef}
        viewBox={`0 0 ${W} ${H}`}
        className="block h-auto w-full touch-none select-none"
        role="img"
        aria-label={`Service map: ${nodes.length} nodes, ${edges.length} flows. The network costs table lists the same flows.`}
        onMouseDown={(e) => {
          if ((e.target as Element).closest("[data-node]")) return;
          const p = toLocal(e.clientX, e.clientY);
          drag.current = { x: p.x, y: p.y, vx: view.x, vy: view.y };
          setDragging(true);
        }}
        onMouseMove={(e) => {
          // Captured now: the updater may run after mouseup cleared the ref.
          const d = drag.current;
          if (!d) return;
          const p = toLocal(e.clientX, e.clientY);
          setView((v) => ({ ...v, x: d.vx + (p.x - d.x), y: d.vy + (p.y - d.y) }));
        }}
        onMouseUp={() => {
          drag.current = null;
          setDragging(false);
        }}
        onMouseLeave={() => {
          drag.current = null;
          setDragging(false);
          setHoverEdge(null);
        }}
        style={{ cursor: dragging ? "grabbing" : "grab" }}
      >
        <defs>
          {(["internal", "cross-zone", "egress"] as const).map((c) => (
            <marker key={c} id={`arrow-${c}`} viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
              <path d="M0,0 L8,4 L0,8 z" fill={EDGE_COLOR[c]} />
            </marker>
          ))}
        </defs>
        <g transform={`translate(${view.x},${view.y}) scale(${view.k})`}>
          {/* zone bands */}
          {[...cols.entries()].map(([zone, x], i, arr) => {
            const w = arr.length > 1 ? Math.abs(arr[1][1] - arr[0][1]) : W * 0.6;
            return (
              <g key={zone}>
                <rect x={x - w / 2 + 6} y={8} width={w - 12} height={H - 16} fill="var(--color-fg)" opacity={i % 2 === 0 ? 0.018 : 0.03} rx={2} />
                <text x={x} y={26} textAnchor="middle" className="fill-[var(--color-fg-faint)] font-mono text-[11px] uppercase tracking-[0.14em]">
                  {zone}
                </text>
              </g>
            );
          })}
          {nodes.some((n) => n.kind === "external") && (
            <text x={W * 0.93} y={26} textAnchor="middle" className="fill-[var(--color-fg-faint)] font-mono text-[11px] uppercase tracking-[0.14em]">
              internet
            </text>
          )}

          {/* edges */}
          {edges.map((e) => {
            const s = byId.get(e.source);
            const t = byId.get(e.target);
            if (!s || !t) return null;
            const cls = edgeClass(e);
            const both = pairs.has(`${e.target}|${e.source}`);
            const p = edgePath(s, t, both ? 1 : 0.15);
            const faded = neighbours && !(neighbours.has(e.source) && neighbours.has(e.target));
            return (
              <g key={e.id} opacity={faded ? 0.12 : 1}>
                <path
                  d={p.d}
                  fill="none"
                  stroke={EDGE_COLOR[cls]}
                  strokeOpacity={cls === "internal" ? 0.55 : 0.85}
                  strokeWidth={edgeWidth(e.bytesPerSec, maxBps)}
                  strokeDasharray={retransmitHeavy(e) ? "5 4" : undefined}
                  markerEnd={`url(#arrow-${cls})`}
                />
                {/* fat invisible hit path */}
                <path
                  d={p.d}
                  fill="none"
                  stroke="transparent"
                  strokeWidth={12}
                  onMouseMove={(ev) => {
                    const r = svgRef.current?.getBoundingClientRect();
                    if (r) setHoverEdge({ e, x: ev.clientX - r.left, y: ev.clientY - r.top });
                  }}
                  onMouseLeave={() => setHoverEdge(null)}
                />
              </g>
            );
          })}

          {/* nodes */}
          {placed.map((n) => {
            const faded = neighbours && !neighbours.has(n.id);
            const isSel = n.id === selected;
            const tone = n.kind === "external" ? "var(--color-accent)" : n.kind === "service" ? "var(--color-cool)" : "var(--color-fg-dim)";
            return (
              <g
                key={n.id}
                data-node
                role="button"
                tabIndex={0}
                aria-label={`${n.namespace ? `${n.namespace}/` : ""}${n.name}, ${n.kind}${n.zone ? ` in ${n.zone}` : ""}, ${formatBytes(n.bytesIn + n.bytesOut)} traffic, ${formatUsd(n.costUsdMonth)} per month`}
                transform={`translate(${n.x},${n.y})`}
                opacity={faded ? 0.25 : 1}
                className="cursor-pointer outline-none focus-visible:[&>circle]:stroke-[var(--color-cool)]"
                onClick={() => setSelected(n.id)}
                onKeyDown={(e) => {
                  if (e.key === "Enter" || e.key === " ") {
                    e.preventDefault();
                    setSelected(n.id);
                  }
                }}
                onMouseEnter={() => setHoverNode(n.id)}
                onMouseLeave={() => setHoverNode(null)}
              >
                <circle r={n.r} fill="var(--color-bg-raised)" stroke={isSel ? "var(--color-fg)" : tone} strokeWidth={isSel ? 2.5 : 1.5} />
                {n.costUsdMonth > 0 && <circle r={Math.max(2, n.r * 0.35)} fill={n.kind === "external" ? "var(--color-accent)" : "var(--color-warn)"} opacity={0.8} />}
                <text y={n.r + 13} textAnchor="middle" className="fill-[var(--color-fg)] font-mono text-[11px]">
                  {n.name.length > 26 ? `${n.name.slice(0, 25)}…` : n.name}
                </text>
                {n.namespace && (
                  <text y={n.r + 25} textAnchor="middle" className="fill-[var(--color-fg-faint)] font-mono text-[9.5px]">
                    {n.namespace}
                  </text>
                )}
              </g>
            );
          })}
        </g>
      </svg>

      {hoverEdge && <EdgeTip edge={hoverEdge.e} x={hoverEdge.x} y={hoverEdge.y} byId={byId} />}

      <div className="mt-2 flex flex-wrap items-center gap-4 font-mono text-[10px] text-[var(--color-fg-faint)]">
        <Key color={EDGE_COLOR.internal}>in-zone</Key>
        <Key color={EDGE_COLOR["cross-zone"]}>cross-zone ($)</Key>
        <Key color={EDGE_COLOR.egress}>internet egress ($$)</Key>
        <Key color="var(--color-fg-dim)" dashed>retransmit-heavy (&gt;0.5%)</Key>
        <span>node size ∝ √traffic · edge width ∝ √bytes/s · wheel+⌘ to zoom, drag to pan</span>
      </div>

      <Drawer
        open={!!selectedNode}
        onClose={() => setSelected(null)}
        title={selectedNode ? `${selectedNode.namespace ? `${selectedNode.namespace}/` : ""}${selectedNode.name}` : ""}
        subtitle={selectedNode ? `${selectedNode.kind}${selectedNode.zone ? ` · ${selectedNode.zone}` : ""} · ${formatBytes(selectedNode.bytesIn)} in · ${formatBytes(selectedNode.bytesOut)} out` : undefined}
      >
        {selectedNode && sel && (
          <div className="flex flex-col gap-5 px-5 py-4">
            <div className="grid grid-cols-2 gap-px border border-[var(--color-line)] bg-[var(--color-line)]">
              <Cell label="network $/mo (sent)" value={formatUsd(selectedNode.costUsdMonth, { cents: true })} tone={selectedNode.costUsdMonth > 0 ? "var(--color-warn)" : undefined} />
              <Cell label="flows" value={`${sel.inbound.length} in · ${sel.outbound.length} out`} />
            </div>
            <EdgeList title="/// outbound" edges={sel.outbound} other={(e) => e.target} byId={byId} />
            <EdgeList title="/// inbound" edges={sel.inbound} other={(e) => e.source} byId={byId} />
            {selectedNode.kind === "workload" && cluster && selectedNode.namespace && (
              <div className="flex flex-wrap gap-2">
                <DrawerLink href={workloadHref(cluster, selectedNode.namespace, selectedNode.name)}>workload hub</DrawerLink>
                <DrawerLink href={logsHref(logqlSelector({ cluster, namespace: selectedNode.namespace, workload: selectedNode.name }), { window: "1h" })}>logs</DrawerLink>
                <DrawerLink href={`/profiles?service=${encodeURIComponent(selectedNode.name)}&namespace=${encodeURIComponent(selectedNode.namespace)}`}>profile</DrawerLink>
              </div>
            )}
          </div>
        )}
      </Drawer>
    </div>
  );
}

function EdgeTip({ edge: e, x, y, byId }: { edge: MapEdge; x: number; y: number; byId: Map<string, MapNode> }) {
  const packets = Math.max(1, e.bytes / 1460);
  return (
    <div
      role="tooltip"
      className="pointer-events-none absolute z-10 w-[300px] border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)]/95 px-3 py-2 shadow-xl"
      style={{ left: Math.min(x + 14, 900), top: y + 14 }}
    >
      <div className="mb-1 break-all font-mono text-[11.5px] text-[var(--color-fg)]">
        {byId.get(e.source)?.name} → {byId.get(e.target)?.name}
        <span className="text-[var(--color-fg-faint)]"> :{e.port}/{e.protocol}</span>
      </div>
      <TipRow k="traffic" v={`${formatBytes(e.bytes)} · ${formatBytes(e.bytesPerSec)}/s`} />
      <TipRow k="cost" v={`${formatUsd(e.costUsdMonth, { cents: true })}/mo`} tone={e.costUsdMonth > 0 ? "var(--color-warn)" : undefined} />
      <TipRow k="class" v={edgeClass(e)} />
      <TipRow k="retransmits" v={`${((e.retransmits / packets) * 100).toFixed(2)}% of packets`} tone={retransmitHeavy(e) ? "var(--color-warn)" : undefined} />
    </div>
  );
}

function TipRow({ k, v, tone }: { k: string; v: string; tone?: string }) {
  return (
    <div className="flex justify-between gap-3 font-mono text-[11px] leading-5">
      <span className="text-[var(--color-fg-faint)]">{k}</span>
      <span className="tabular-nums" style={{ color: tone ?? "var(--color-fg-dim)" }}>
        {v}
      </span>
    </div>
  );
}

function EdgeList({ title, edges, other, byId }: { title: string; edges: MapEdge[]; other: (e: MapEdge) => string; byId: Map<string, MapNode> }) {
  return (
    <section>
      <h3 className="section-label mb-2">{title} · {edges.length}</h3>
      {edges.length === 0 ? (
        <div className="font-mono text-[11px] text-[var(--color-fg-faint)]">none</div>
      ) : (
        <table className="w-full border-collapse font-mono text-[11.5px]">
          <tbody>
            {edges.map((e) => {
              const cls = edgeClass(e);
              return (
                <tr key={e.id} className="border-b border-[var(--color-line)] last:border-b-0">
                  <td className="py-1.5 pr-2">
                    <span className="inline-block h-2 w-2" style={{ background: EDGE_COLOR[cls] }} aria-hidden />
                  </td>
                  <td className="max-w-[220px] truncate py-1.5 pr-2 text-[var(--color-fg)]">{byId.get(other(e))?.name ?? other(e)}</td>
                  <td className="py-1.5 pr-2 text-[var(--color-fg-faint)]">:{e.port}</td>
                  <td className="py-1.5 pr-2 text-right tabular-nums text-[var(--color-fg-dim)]">{formatGB(e.bytes / 1e9)}</td>
                  <td className="py-1.5 text-right tabular-nums" style={{ color: e.costUsdMonth > 0 ? "var(--color-warn)" : "var(--color-fg-faint)" }}>
                    {e.costUsdMonth > 0 ? `${formatUsd(e.costUsdMonth, { cents: true })}/mo` : "—"}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Cell({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="flex flex-col gap-1 bg-[var(--color-bg-raised)] px-3 py-2.5">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">{label}</span>
      <span className="font-mono text-[13px] tabular-nums" style={{ color: tone ?? "var(--color-fg)" }}>
        {value}
      </span>
    </div>
  );
}

function Key({ color, dashed, children }: { color: string; dashed?: boolean; children: React.ReactNode }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <svg width="18" height="6" aria-hidden>
        <line x1="0" y1="3" x2="18" y2="3" stroke={color} strokeWidth="2" strokeDasharray={dashed ? "4 3" : undefined} />
      </svg>
      {children}
    </span>
  );
}

function MapButton({ label, onClick, icon: Icon }: { label: string; onClick: () => void; icon: React.ComponentType<{ className?: string }> }) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      title={label}
      className="bg-[var(--color-bg-raised)] p-1.5 text-[var(--color-fg-dim)] transition-colors hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
    >
      <Icon className="h-3.5 w-3.5" aria-hidden />
    </button>
  );
}

function DrawerLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
    >
      {children}
    </Link>
  );
}
