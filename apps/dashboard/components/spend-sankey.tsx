"use client";

import Link from "next/link";
import { useMemo, useState } from "react";
import { Download, ExternalLink, X } from "lucide-react";

/* ───────────────────────────────────────────────────────────────────────────
   SPEND SANKEY — namespace → workload → cloud

   3-column flow diagram. Ribbon thickness = $/mo.

     · hover any node/ribbon  → focus the flow that touches it
     · click a node/ribbon    → pin the filter + open the detail panel
     · drag the threshold     → hide flows below $X / mo (declutter)
     · 24h · 7d · 30d         → rescale the values displayed
     · "export CSV"           → download the currently-visible flows

   Engineered for two readers:
     · noob: big obvious flows, plain-English labels, hover = tooltip.
     · FinOps engineer: exact $ per path, scannable totals, click-to-pin.
   ─────────────────────────────────────────────────────────────────────────── */

type Window = "24h" | "7d" | "30d";

// 24h ≈ 30d / 30 (linear approx); 7d ≈ 30d * 7/30. The chart still labels
// the time bucket explicitly so this stays honest.
const WINDOW_MULTIPLIER: Record<Window, number> = {
  "24h": 1 / 30,
  "7d": 7 / 30,
  "30d": 1,
};

type Kind = "namespace" | "workload" | "cloud";

type Flow = {
  id: string;
  left: string;  // namespace id
  mid: string;   // workload id
  right: string; // cloud id
  monthly: number; // USD / mo
};

type Node = {
  id: string;
  label: string;
  kind: Kind;
  tone?: string;
};

const NAMESPACES: Node[] = [
  { id: "ml", label: "ml-inference", kind: "namespace" },
  { id: "retrieval", label: "retrieval", kind: "namespace" },
  { id: "data", label: "data", kind: "namespace" },
  { id: "edge", label: "edge", kind: "namespace" },
  { id: "platform", label: "platform", kind: "namespace" },
];

const WORKLOADS: Node[] = [
  { id: "model-server", label: "model-server-a100", kind: "workload", tone: "var(--color-accent)" },
  { id: "vectordb", label: "vectordb-ingress", kind: "workload", tone: "var(--color-accent)" },
  { id: "etl", label: "etl-nightly", kind: "workload", tone: "var(--color-warn)" },
  { id: "frontend", label: "frontend-gateway", kind: "workload" },
  { id: "api", label: "api-ingress", kind: "workload" },
  { id: "autoscaler", label: "cluster-autoscaler", kind: "workload" },
];

const CLOUDS: Node[] = [
  { id: "aws", label: "AWS", kind: "cloud", tone: "var(--color-warn)" },
  { id: "gcp", label: "GCP", kind: "cloud", tone: "var(--color-signal)" },
  { id: "azure", label: "Azure", kind: "cloud", tone: "var(--color-cool)" },
];

// Flows: namespace → workload → cloud, values in USD / month.
const FLOWS: Flow[] = [
  { id: "f1",  left: "ml",        mid: "model-server", right: "aws",   monthly: 45000 },
  { id: "f2",  left: "ml",        mid: "model-server", right: "azure", monthly: 37000 },
  { id: "f3",  left: "retrieval", mid: "vectordb",     right: "aws",   monthly: 22000 },
  { id: "f4",  left: "retrieval", mid: "vectordb",     right: "gcp",   monthly: 12000 },
  { id: "f5",  left: "data",      mid: "etl",          right: "gcp",   monthly: 16000 },
  { id: "f6",  left: "data",      mid: "etl",          right: "aws",   monthly:  6000 },
  { id: "f7",  left: "edge",      mid: "frontend",     right: "aws",   monthly:  8000 },
  { id: "f8",  left: "edge",      mid: "frontend",     right: "gcp",   monthly:  3000 },
  { id: "f9",  left: "edge",      mid: "frontend",     right: "azure", monthly:  1000 },
  { id: "f10", left: "edge",      mid: "api",          right: "azure", monthly:  7000 },
  { id: "f11", left: "edge",      mid: "api",          right: "aws",   monthly:  2000 },
  { id: "f12", left: "platform",  mid: "autoscaler",   right: "aws",   monthly:  2000 },
  { id: "f13", left: "platform",  mid: "autoscaler",   right: "gcp",   monthly:  1500 },
  { id: "f14", left: "platform",  mid: "autoscaler",   right: "azure", monthly:   500 },
];

// Layout
const W = 1040;
const H = 440;
const COL_W = 120;        // node column width
const PAD_Y = 8;          // min gap between stacked nodes
const GAP = 4;            // min pixels per node
const leftX = 0;
const midX = (W - COL_W) / 2;
const rightX = W - COL_W;

type Layout = {
  nodes: Record<string, { x: number; y: number; h: number; label: string; kind: Kind; total: number; tone?: string }>;
  ribbons: {
    id: string;
    d: string;
    value: number;
    width: number;
    flow: Flow;
    left: string;
    right: string;
    mid: string;
  }[];
  totalMonthly: number;
  scale: number; // px per $
};

function computeLayout(opts: {
  thresholdMonthly: number;
  multiplier: number;
}): Layout {
  const { thresholdMonthly, multiplier } = opts;

  // Apply window multiplier to flows + filter below threshold.
  const visibleFlows: Flow[] = FLOWS
    .map((f) => ({ ...f, monthly: f.monthly * multiplier }))
    .filter((f) => f.monthly >= thresholdMonthly);

  // Sum totals per node
  const totalByNode: Record<string, number> = {};
  for (const f of visibleFlows) {
    totalByNode[f.left] = (totalByNode[f.left] ?? 0) + f.monthly;
    totalByNode[f.mid] = (totalByNode[f.mid] ?? 0) + f.monthly;
    totalByNode[f.right] = (totalByNode[f.right] ?? 0) + f.monthly;
  }
  const totalMonthly = visibleFlows.reduce((a, b) => a + b.monthly, 0);

  // Only render columns with at least one visible node so the layout
  // doesn't waste space on empty rows.
  const visibleNs = NAMESPACES.filter((n) => (totalByNode[n.id] ?? 0) > 0);
  const visibleW = WORKLOADS.filter((n) => (totalByNode[n.id] ?? 0) > 0);
  const visibleC = CLOUDS.filter((n) => (totalByNode[n.id] ?? 0) > 0);

  // Scale: (H - total gaps) / totalMonthly per column
  const maxNodes = Math.max(visibleNs.length, visibleW.length, visibleC.length, 1);
  const availableH = H - PAD_Y * (maxNodes - 1);
  const scale = totalMonthly > 0 ? availableH / totalMonthly : 0;

  const place = (col: Node[], x: number) => {
    let y = 0;
    const out: Layout["nodes"] = {};
    for (const n of col) {
      const total = totalByNode[n.id] ?? 0;
      if (total === 0) continue;
      const h = Math.max(GAP, total * scale);
      out[n.id] = { x, y, h, label: n.label, kind: n.kind, total, tone: n.tone };
      y += h + PAD_Y;
    }
    return out;
  };
  const nodes = {
    ...place(visibleNs, leftX),
    ...place(visibleW, midX),
    ...place(visibleC, rightX),
  };

  // Pack ribbons within each node. We track cumulative y on each side of each node.
  const cursor: Record<string, { left: number; right: number }> = {};
  for (const id of Object.keys(nodes)) cursor[id] = { left: 0, right: 0 };

  const sortedFlows = [...visibleFlows].sort((a, b) => b.monthly - a.monthly);

  const ribbons: Layout["ribbons"] = sortedFlows.flatMap((f) => {
    if (!nodes[f.left] || !nodes[f.mid] || !nodes[f.right]) return [];
    const segA = ribbon(
      nodes[f.left],
      nodes[f.mid],
      f.monthly,
      scale,
      cursor[f.left].right,
      cursor[f.mid].left,
      /* sourceSide */ "right",
      /* targetSide */ "left",
      COL_W,
    );
    cursor[f.left].right += segA.thickness;
    cursor[f.mid].left += segA.thickness;

    const segB = ribbon(
      nodes[f.mid],
      nodes[f.right],
      f.monthly,
      scale,
      cursor[f.mid].right,
      cursor[f.right].left,
      "right",
      "left",
      COL_W,
    );
    cursor[f.mid].right += segB.thickness;
    cursor[f.right].left += segB.thickness;

    return [
      {
        id: f.id + "-a",
        d: segA.d,
        value: f.monthly,
        width: segA.thickness,
        flow: f,
        left: f.left,
        right: f.mid,
        mid: f.mid,
      },
      {
        id: f.id + "-b",
        d: segB.d,
        value: f.monthly,
        width: segB.thickness,
        flow: f,
        left: f.mid,
        right: f.right,
        mid: f.mid,
      },
    ];
  });

  return { nodes, ribbons, totalMonthly, scale };
}

function ribbon(
  a: Layout["nodes"][string],
  b: Layout["nodes"][string],
  value: number,
  scale: number,
  srcCursor: number,
  dstCursor: number,
  _s: "right",
  _t: "left",
  colW: number,
): { d: string; thickness: number } {
  const thickness = Math.max(1.5, value * scale);
  const x1 = a.x + colW;
  const x2 = b.x;
  const y1 = a.y + srcCursor + thickness / 2;
  const y2 = b.y + dstCursor + thickness / 2;
  const cx = (x1 + x2) / 2;
  const d = `M${x1} ${y1} C${cx} ${y1}, ${cx} ${y2}, ${x2} ${y2}`;
  return { d, thickness };
}

/* ──────────── component ──────────── */

export function SpendSankey() {
  const [windowSel, setWindowSel] = useState<Window>("30d");
  const [threshold, setThreshold] = useState<number>(0);

  const layout = useMemo(
    () => computeLayout({ thresholdMonthly: threshold, multiplier: WINDOW_MULTIPLIER[windowSel] }),
    [windowSel, threshold],
  );
  const [hover, setHover] = useState<string | null>(null);
  const [pin, setPin] = useState<string | null>(null);
  const focus = pin ?? hover;

  // Which ribbons + nodes should be highlighted based on focus?
  const highlight = useMemo(() => {
    if (!focus) return { ribbons: null, nodes: null } as {
      ribbons: Set<string> | null;
      nodes: Set<string> | null;
    };
    const keptRibbons = new Set<string>();
    const keptNodes = new Set<string>();
    for (const r of layout.ribbons) {
      const f = r.flow;
      if (f.left === focus || f.mid === focus || f.right === focus) {
        keptRibbons.add(r.id);
        keptNodes.add(f.left);
        keptNodes.add(f.mid);
        keptNodes.add(f.right);
      }
    }
    if (keptRibbons.size === 0) return { ribbons: null, nodes: null };
    return { ribbons: keptRibbons, nodes: keptNodes };
  }, [focus, layout.ribbons]);

  const [ribHover, setRibHover] = useState<string | null>(null);
  const [ribPin, setRibPin] = useState<string | null>(null);
  const hoveredRibbon = ribHover
    ? layout.ribbons.find((r) => r.id === ribHover) ?? null
    : null;
  const pinnedRibbon = ribPin
    ? layout.ribbons.find((r) => r.id === ribPin) ?? null
    : null;

  // Detail-panel data: when a node is pinned, list every flow that touches
  // it, sorted by descending value. When a ribbon is pinned, show its flow.
  const detailFlows = useMemo(() => {
    if (pinnedRibbon) return [pinnedRibbon.flow];
    if (!pin) return [];
    const fs = layout.ribbons
      .map((r) => r.flow)
      .filter((f) => f.left === pin || f.mid === pin || f.right === pin);
    // Each flow appears twice (once per ribbon segment); dedupe.
    const seen = new Set<string>();
    return fs.filter((f) => {
      if (seen.has(f.id)) return false;
      seen.add(f.id);
      return true;
    });
  }, [pin, pinnedRibbon, layout.ribbons]);

  const detailTitle = pinnedRibbon
    ? `${labelOf(pinnedRibbon.flow.left)} → ${labelOf(pinnedRibbon.flow.mid)} → ${labelOf(pinnedRibbon.flow.right)}`
    : pin
      ? labelOf(pin)
      : null;

  const detailTotal = detailFlows.reduce(
    (a, f) => a + f.monthly * WINDOW_MULTIPLIER[windowSel],
    0,
  );

  const exportCsv = () => {
    const rows = [
      ["namespace", "workload", "cloud", "amount_usd", "window"],
      ...layout.ribbons
        .filter((r) => r.id.endsWith("-b")) // each flow has -a + -b ribbons; -b carries the cloud edge
        .map((r) => [
          r.flow.left,
          r.flow.mid,
          r.flow.right,
          (r.flow.monthly * WINDOW_MULTIPLIER[windowSel]).toFixed(2),
          windowSel,
        ]),
    ];
    const csv = rows.map((r) => r.join(",")).join("\n");
    const blob = new Blob([csv], { type: "text/csv;charset=utf-8" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `kubehero-spend-${windowSel}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
      {/* chrome row 1 — title + total */}
      <div className="flex flex-wrap items-center gap-3 border-b border-[var(--color-line)] px-4 py-3 md:px-5">
        <div className="flex items-center gap-1.5">
          <span className="h-2 w-2 rounded-full bg-[var(--color-line-bright)]" />
          <span className="h-2 w-2 rounded-full bg-[var(--color-line-bright)]" />
          <span className="h-2 w-2 rounded-full bg-[var(--color-line-bright)]" />
        </div>
        <span className="font-mono text-[11.5px] text-[var(--color-fg)]">
          kubehero / spend attribution
        </span>
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          · namespace → workload → cloud
        </span>
        <div className="ml-auto flex items-center gap-3 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          <span>
            total · {windowSel} ·{" "}
            <span className="text-[var(--color-fg)]">
              ${(layout.totalMonthly / 1000).toFixed(1)}k
            </span>
          </span>
          {(pin || ribPin) && (
            <button
              type="button"
              onClick={() => {
                setPin(null);
                setRibPin(null);
              }}
              className="inline-flex items-center gap-1 text-[var(--color-cool)] hover:text-[var(--color-fg)]"
            >
              <X className="h-3 w-3" /> clear pin
            </button>
          )}
        </div>
      </div>

      {/* chrome row 2 — controls */}
      <div className="flex flex-wrap items-center gap-3 border-b border-[var(--color-line)] bg-[var(--color-bg-sunken)]/40 px-4 py-2 md:px-5">
        <div className="inline-flex items-center gap-0.5 rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-0.5 font-mono text-[10px] uppercase tracking-[0.14em]">
          {(["24h", "7d", "30d"] as const).map((w) => (
            <button
              key={w}
              type="button"
              onClick={() => setWindowSel(w)}
              className="px-2 py-0.5 transition-colors"
              style={{
                background: windowSel === w ? "var(--color-fg)" : "transparent",
                color: windowSel === w ? "var(--color-bg)" : "var(--color-fg-dim)",
              }}
            >
              {w}
            </button>
          ))}
        </div>

        <label className="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          min flow
          <input
            type="range"
            min={0}
            max={20000}
            step={500}
            value={threshold}
            onChange={(e) => setThreshold(Number(e.target.value))}
            className="h-1 w-32 cursor-pointer accent-[var(--color-cool)]"
            aria-label="hide flows below this monthly amount"
          />
          <span className="w-14 tabular-nums text-[var(--color-fg-dim)]">
            ${(threshold / 1000).toFixed(1)}k
          </span>
        </label>

        <button
          type="button"
          onClick={exportCsv}
          className="ml-auto inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
          aria-label="export visible flows as CSV"
        >
          <Download className="h-3 w-3" />
          export csv
        </button>
      </div>

      {/* sankey */}
      <div className="relative px-4 pb-3 pt-5 md:px-6">
        {/* column labels */}
        <div className="mb-2 hidden grid-cols-3 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] md:grid">
          <span>namespace</span>
          <span className="text-center">workload</span>
          <span className="text-right">cloud</span>
        </div>

        {/* ─── mobile list fallback (below md) ─── */}
        <div className="flex flex-col gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] md:hidden">
          {[...FLOWS]
            .sort((a, b) => b.monthly - a.monthly)
            .slice(0, 10)
            .map((f) => (
              <div
                key={f.id}
                className="flex items-center gap-2 bg-[var(--color-bg-sunken)] px-3 py-2.5 font-mono text-[11px]"
              >
                <span
                  className="h-1.5 w-1.5 shrink-0"
                  style={{ background: cloudColor(f.right) }}
                  aria-hidden
                />
                <span className="flex min-w-0 flex-1 flex-col">
                  <span className="truncate text-[var(--color-fg)]">
                    {labelOf(f.mid)}
                  </span>
                  <span className="truncate text-[10px] text-[var(--color-fg-dim)]">
                    <span className="text-[var(--color-fg-faint)]">ns</span>{" "}
                    {labelOf(f.left)}{" "}
                    <span className="text-[var(--color-fg-faint)]">→</span>{" "}
                    <span className="uppercase tracking-[0.1em]">
                      {labelOf(f.right)}
                    </span>
                  </span>
                </span>
                <span className="shrink-0 tabular-nums text-[var(--color-fg)]">
                  ${(f.monthly / 1000).toFixed(1)}k
                </span>
              </div>
            ))}
        </div>

        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="hidden h-auto w-full md:block"
          preserveAspectRatio="xMidYMid meet"
          role="img"
          aria-label="spend attribution from namespace to workload to cloud"
        >
          {/* ribbons — draw first so nodes overlay */}
          <g>
            {layout.ribbons.map((r) => {
              const isOn = highlight.ribbons
                ? highlight.ribbons.has(r.id)
                : true;
              const isHover = ribHover === r.id;
              const isPinned = ribPin === r.id;
              return (
                <path
                  key={r.id}
                  d={r.d}
                  stroke={isPinned || isHover ? "var(--color-fg)" : cloudColor(r.flow.right)}
                  strokeOpacity={isOn ? (isHover || isPinned ? 0.95 : 0.45) : 0.08}
                  strokeWidth={r.width}
                  fill="none"
                  strokeLinecap="butt"
                  onMouseEnter={() => setRibHover(r.id)}
                  onMouseLeave={() => setRibHover(null)}
                  onClick={() => {
                    // Pinning a ribbon clears any node pin; only one pin
                    // lives at a time so the detail panel stays unambiguous.
                    setRibPin((p) => (p === r.id ? null : r.id));
                    setPin(null);
                  }}
                  style={{ cursor: "pointer", transition: "stroke-opacity 160ms linear" }}
                />
              );
            })}
          </g>

          {/* nodes */}
          <g>
            {Object.entries(layout.nodes).map(([id, n]) => {
              const on = highlight.nodes ? highlight.nodes.has(id) : true;
              const active = focus === id;
              return (
                <g
                  key={id}
                  onMouseEnter={() => setHover(id)}
                  onMouseLeave={() => setHover((h) => (h === id ? null : h))}
                  onClick={() => {
                    setPin((p) => (p === id ? null : id));
                    setRibPin(null);
                  }}
                  style={{ cursor: "pointer", opacity: on ? 1 : 0.35 }}
                >
                  <rect
                    x={n.x}
                    y={n.y}
                    width={COL_W}
                    height={n.h}
                    fill={
                      active
                        ? "var(--color-fg)"
                        : n.tone ?? "var(--color-line-bright)"
                    }
                    rx={2}
                  />
                  <text
                    x={
                      n.kind === "cloud"
                        ? n.x - 8
                        : n.kind === "namespace"
                          ? n.x + COL_W + 8
                          : n.x + COL_W / 2
                    }
                    y={n.y + n.h / 2 + 3}
                    textAnchor={
                      n.kind === "cloud"
                        ? "end"
                        : n.kind === "namespace"
                          ? "start"
                          : "middle"
                    }
                    fontFamily="var(--font-mono)"
                    fontSize="11"
                    fill="var(--color-fg)"
                    style={{ pointerEvents: "none" }}
                  >
                    {n.label}
                  </text>
                  <text
                    x={
                      n.kind === "cloud"
                        ? n.x - 8
                        : n.kind === "namespace"
                          ? n.x + COL_W + 8
                          : n.x + COL_W / 2
                    }
                    y={n.y + n.h / 2 + 16}
                    textAnchor={
                      n.kind === "cloud"
                        ? "end"
                        : n.kind === "namespace"
                          ? "start"
                          : "middle"
                    }
                    fontFamily="var(--font-mono)"
                    fontSize="9.5"
                    fill="var(--color-fg-faint)"
                    style={{ pointerEvents: "none" }}
                  >
                    ${(n.total / 1000).toFixed(1)}k
                  </text>
                </g>
              );
            })}
          </g>
        </svg>

        {/* ribbon tooltip — desktop only, suppressed when a panel is pinned */}
        {hoveredRibbon && !pin && !ribPin && (
          <div className="pointer-events-none absolute left-1/2 top-2 hidden -translate-x-1/2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-1.5 font-mono text-[11px] leading-tight md:block">
            <span className="text-[var(--color-fg-dim)]">
              {labelOf(hoveredRibbon.flow.left)}{" "}
              <span className="text-[var(--color-fg-faint)]">→</span>{" "}
              {labelOf(hoveredRibbon.flow.mid)}{" "}
              <span className="text-[var(--color-fg-faint)]">→</span>{" "}
              {labelOf(hoveredRibbon.flow.right)}
            </span>
            <span className="ml-3 tabular-nums text-[var(--color-fg)]">
              ${(hoveredRibbon.value / 1000).toFixed(1)}k / {windowSel}
            </span>
          </div>
        )}
      </div>

      {/* detail panel — shown when a node or ribbon is pinned */}
      {detailTitle && (
        <div className="border-t border-[var(--color-line)] bg-[var(--color-bg-sunken)]/40 px-4 py-3 md:px-5">
          <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>/// pinned ·</span>
            <span className="text-[var(--color-fg)]">{detailTitle}</span>
            <span className="ml-auto tabular-nums">
              total{" "}
              <span className="text-[var(--color-fg)]">
                ${(detailTotal / 1000).toFixed(1)}k / {windowSel}
              </span>
              {" · "}
              {detailFlows.length} flow{detailFlows.length === 1 ? "" : "s"}
            </span>
          </div>
          <div className="grid gap-[1px] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-3">
            {detailFlows.map((f) => {
              const amount = f.monthly * WINDOW_MULTIPLIER[windowSel];
              const wnode = WORKLOADS.find((w) => w.id === f.mid);
              // /workloads/[cluster]/[ns]/[name] — we don't carry cluster
              // on the demo flows so use the cloud as a stand-in slug.
              const href = `/workloads/${encodeURIComponent(f.right)}/${encodeURIComponent(f.left)}/${encodeURIComponent(wnode?.label ?? f.mid)}`;
              return (
                <Link
                  key={f.id}
                  href={href}
                  className="flex items-baseline justify-between gap-3 bg-[var(--color-bg-raised)] px-3 py-2 font-mono text-[11px] transition-colors hover:bg-[var(--color-bg-raised)]/60"
                >
                  <span className="flex min-w-0 flex-col">
                    <span className="truncate text-[var(--color-fg)]">
                      {labelOf(f.mid)}
                    </span>
                    <span className="truncate text-[10px] text-[var(--color-fg-dim)]">
                      <span className="text-[var(--color-fg-faint)]">ns</span>{" "}
                      {labelOf(f.left)}{" "}
                      <span className="text-[var(--color-fg-faint)]">→</span>{" "}
                      <span style={{ color: cloudColor(f.right) }}>
                        {labelOf(f.right)}
                      </span>
                    </span>
                  </span>
                  <span className="flex items-center gap-1.5 text-[var(--color-fg)]">
                    <span className="tabular-nums">${(amount / 1000).toFixed(1)}k</span>
                    <ExternalLink className="h-3 w-3 text-[var(--color-fg-faint)]" />
                  </span>
                </Link>
              );
            })}
          </div>
        </div>
      )}

      {/* footer */}
      <div className="flex flex-wrap items-center justify-between gap-3 border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] md:px-5">
        <span>
          {pin || ribPin
            ? `pinned · click again to release · ribbon-click drills into a single flow`
            : "hover to focus · click a node or ribbon to pin · drag the threshold to declutter"}
        </span>
        <span className="text-[var(--color-cool)]">
          source · ch.pod_cost_1d · GROUP BY ns, workload, cloud
        </span>
      </div>
    </div>
  );
}

/* ──────────── helpers ──────────── */

function cloudColor(id: string): string {
  switch (id) {
    case "aws":
      return "var(--color-warn)";
    case "gcp":
      return "var(--color-signal)";
    case "azure":
      return "var(--color-cool)";
    default:
      return "var(--color-fg-dim)";
  }
}

function labelOf(id: string): string {
  const all = [...NAMESPACES, ...WORKLOADS, ...CLOUDS];
  return all.find((n) => n.id === id)?.label ?? id;
}
