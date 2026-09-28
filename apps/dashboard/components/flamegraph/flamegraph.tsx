// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Canvas icicle flamegraph. All geometry comes from lib/flame/tree.ts
// (x-extents in root units, computed once); the canvas only maps a
// viewport [a, b] onto pixels, so zoom is a single animated lerp of two
// numbers — smooth at 60fps even with thousands of frames. Search dims
// non-matching frames and reports the share of time they cover; diff
// mode paints regressions red and improvements green by how much each
// frame's share of total moved. Tooltips price every frame in $/month.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useReducedMotion } from "motion/react";
import { RotateCcw, Search, X } from "lucide-react";
import {
  ancestry,
  buildTree,
  compileSearch,
  diffColor,
  diffOf,
  frameColor,
  frameCost,
  hitTest,
  relativeChange,
  searchTree,
  visibleRects,
  type FlameNode,
} from "@/lib/flame/tree";
import { formatProfileValue } from "@/lib/profiles/map";
import { formatUsd } from "@/lib/chart/scale";
import { useWidth } from "@/components/charts/use-width";

const ROW = 20;
const GAP = 1;
const FONT = '11px "JetBrains Mono Variable", "JetBrains Mono", ui-monospace, monospace';

export function Flamegraph({
  nodes,
  unit,
  costUsdMonth,
  baselineTotal = 0,
  highlight = "",
  maxHeight = 640,
}: {
  nodes: FlameNode[];
  unit: string;
  costUsdMonth: number;
  /** > 0 switches on diff colouring. */
  baselineTotal?: number;
  highlight?: string;
  maxHeight?: number;
}) {
  const tree = useMemo(() => buildTree(nodes), [nodes]);
  const rootTotal = nodes[0]?.total ?? 0;
  const diff = baselineTotal > 0;
  const reduce = useReducedMotion();
  const [wrapRef, width] = useWidth<HTMLDivElement>();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [focus, setFocus] = useState(0);
  const [view, setView] = useState<[number, number]>([0, 1]);
  const [hover, setHover] = useState<{ i: number; x: number; y: number } | null>(null);
  const [query, setQuery] = useState(highlight);
  const anim = useRef<number | null>(null);
  const snap = useRef<ReturnType<typeof setTimeout> | null>(null);
  const viewRef = useRef<[number, number]>([0, 1]);
  const textCache = useRef(new Map<string, string>());

  const matcher = useMemo(() => compileSearch(query), [query]);
  const search = useMemo(() => (matcher ? searchTree(tree, matcher) : null), [tree, matcher]);
  const path = useMemo(() => ancestry(tree, focus), [tree, focus]);
  const onPath = useMemo(() => new Set(path), [path]);
  const height = (tree.maxDepth + 1) * ROW;

  // Animate the viewport to the focused node's extent.
  const zoomTo = useCallback(
    (i: number) => {
      setFocus(i);
      const target: [number, number] = [tree.x0[i], tree.x1[i]];
      if (anim.current) cancelAnimationFrame(anim.current);
      if (reduce) {
        viewRef.current = target;
        setView(target);
        return;
      }
      const from = viewRef.current;
      const t0 = performance.now();
      const dur = 220;
      const step = (now: number) => {
        const k = Math.min(1, (now - t0) / dur);
        const e = 1 - Math.pow(1 - k, 3);
        const v: [number, number] = [from[0] + (target[0] - from[0]) * e, from[1] + (target[1] - from[1]) * e];
        viewRef.current = v;
        setView(v);
        if (k < 1) anim.current = requestAnimationFrame(step);
      };
      anim.current = requestAnimationFrame(step);
      // rAF pauses in background tabs; never leave the view half-zoomed.
      if (snap.current) clearTimeout(snap.current);
      snap.current = setTimeout(() => {
        if (viewRef.current[0] !== target[0] || viewRef.current[1] !== target[1]) {
          if (anim.current) cancelAnimationFrame(anim.current);
          viewRef.current = target;
          setView(target);
        }
      }, dur + 80);
    },
    [tree, reduce],
  );

  useEffect(
    () => () => {
      if (anim.current) cancelAnimationFrame(anim.current);
      if (snap.current) clearTimeout(snap.current);
    },
    [],
  );

  // Draw.
  useEffect(() => {
    const cv = canvasRef.current;
    if (!cv || width === 0) return;
    const dpr = typeof window !== "undefined" ? Math.min(2, window.devicePixelRatio || 1) : 1;
    cv.width = Math.floor(width * dpr);
    cv.height = Math.floor(height * dpr);
    cv.style.width = `${width}px`;
    cv.style.height = `${height}px`;
    const ctx = cv.getContext("2d");
    if (!ctx) return;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);
    ctx.font = FONT;
    ctx.textBaseline = "middle";
    const [a, b] = view;
    const rects = visibleRects(tree, a, b, width, 0.35);
    const focusDepth = nodes[focus]?.depth ?? 0;
    for (const r of rects) {
      const n = nodes[r.i];
      const above = r.depth < focusDepth; // ancestors of the zoomed frame
      const dim = (search !== null && !search.matched.has(r.i)) || (above && !onPath.has(r.i));
      let fill: string;
      if (diff) fill = diffColor(diffOf(n, rootTotal, baselineTotal));
      else fill = frameColor(n.name, dim);
      if (diff && dim) fill = "hsl(40 4% 16%)";
      ctx.globalAlpha = above ? 0.55 : 1;
      ctx.fillStyle = fill;
      const y = r.depth * ROW;
      ctx.fillRect(r.x, y, Math.max(0.5, r.w - GAP), ROW - GAP);
      if (search?.matched.has(r.i) && !diff) {
        ctx.globalAlpha = 1;
        ctx.strokeStyle = "rgba(255, 210, 120, 0.9)";
        ctx.lineWidth = 1;
        ctx.strokeRect(r.x + 0.5, y + 0.5, Math.max(0, r.w - GAP - 1), ROW - GAP - 1);
      }
      if (r.w > 26) {
        ctx.globalAlpha = dim ? 0.45 : above ? 0.7 : 0.92;
        ctx.fillStyle = "#f4f4f2";
        ctx.fillText(fit(ctx, textCache.current, n.name, r.w - 8), r.x + 4, y + (ROW - GAP) / 2 + 0.5);
      }
    }
    ctx.globalAlpha = 1;
    if (hover) {
      const hi = hover.i;
      const r = rects.find((x) => x.i === hi);
      if (r) {
        ctx.strokeStyle = "#ffffff";
        ctx.lineWidth = 1;
        ctx.strokeRect(r.x + 0.5, r.depth * ROW + 0.5, Math.max(0, r.w - GAP - 1), ROW - GAP - 1);
      }
    }
  }, [tree, nodes, view, width, height, search, hover, focus, onPath, diff, rootTotal, baselineTotal]);

  const locate = (e: React.MouseEvent<HTMLCanvasElement>): { i: number; x: number; y: number } | null => {
    const rect = e.currentTarget.getBoundingClientRect();
    const x = e.clientX - rect.left;
    const y = e.clientY - rect.top;
    const depth = Math.floor(y / ROW);
    const [a, b] = viewRef.current;
    const rx = a + (x / Math.max(1, width)) * (b - a);
    const i = hitTest(tree, depth, rx);
    return i >= 0 ? { i, x, y } : null;
  };

  const n = hover ? nodes[hover.i] : null;
  const pct = (v: number) => (rootTotal > 0 ? `${((v / rootTotal) * 100).toFixed(2)}%` : "—");

  return (
    <div className="min-w-0">
      {/* toolbar: breadcrumb · search · reset */}
      <div className="mb-2 flex flex-wrap items-center gap-2">
        <nav aria-label="Zoom path" className="flex min-w-0 flex-1 flex-wrap items-center gap-1 font-mono text-[11px]">
          {path.map((i, k) => (
            <span key={i} className="inline-flex min-w-0 items-center gap-1">
              {k > 0 && <span className="text-[var(--color-fg-faint)]">›</span>}
              <button
                type="button"
                onClick={() => zoomTo(i)}
                className={`max-w-[220px] truncate hover:text-[var(--color-cool)] ${i === focus ? "text-[var(--color-fg)]" : "text-[var(--color-fg-dim)]"}`}
                title={nodes[i].name}
              >
                {k === 0 ? "all" : shortName(nodes[i].name)}
              </button>
            </span>
          ))}
        </nav>
        <label className="flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 focus-within:border-[var(--color-cool)]">
          <Search className="h-3 w-3 text-[var(--color-fg-faint)]" aria-hidden />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="search frames · /regex/"
            aria-label="Search frames"
            spellCheck={false}
            className="w-44 bg-transparent font-mono text-[11.5px] text-[var(--color-fg)] placeholder:text-[var(--color-fg-faint)] focus:outline-none"
          />
          {query && (
            <button type="button" onClick={() => setQuery("")} aria-label="Clear search" className="text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]">
              <X className="h-3 w-3" aria-hidden />
            </button>
          )}
        </label>
        {search && (
          <span className="font-mono text-[10.5px] tabular-nums text-[var(--color-warn)]" role="status">
            {search.matched.size} frames · {(search.fraction * 100).toFixed(1)}% of total
          </span>
        )}
        <button
          type="button"
          onClick={() => zoomTo(0)}
          disabled={focus === 0}
          className="inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:text-[var(--color-fg)] disabled:opacity-40"
        >
          <RotateCcw className="h-3 w-3" aria-hidden /> reset
        </button>
      </div>

      {diff && (
        <div className="mb-2 flex flex-wrap items-center gap-3 font-mono text-[10px] text-[var(--color-fg-faint)]">
          <span className="inline-flex items-center gap-1.5"><span className="h-2 w-4" style={{ background: "hsl(2 80% 46%)" }} aria-hidden /> slower than baseline</span>
          <span className="inline-flex items-center gap-1.5"><span className="h-2 w-4" style={{ background: "hsl(146 80% 42%)" }} aria-hidden /> faster</span>
          <span>colour strength = change in share of total (saturates at ±5pp)</span>
        </div>
      )}

      <div ref={wrapRef} className="relative overflow-y-auto overflow-x-hidden" style={{ maxHeight }}>
        <canvas
          ref={canvasRef}
          role="img"
          tabIndex={0}
          aria-label={`Flamegraph, ${nodes.length} frames. Click a frame to zoom, Escape to zoom out. The top functions table below lists the same data.`}
          className="block cursor-pointer outline-none focus-visible:ring-1 focus-visible:ring-[var(--color-cool)]"
          style={{ height }}
          onMouseMove={(e) => setHover(locate(e))}
          onMouseLeave={() => setHover(null)}
          onClick={(e) => {
            const hit = locate(e);
            if (hit) zoomTo(hit.i === focus && hit.i !== 0 ? nodes[hit.i].parent : hit.i);
          }}
          onKeyDown={(e) => {
            if (e.key === "Escape" && focus !== 0) {
              e.preventDefault();
              zoomTo(Math.max(0, nodes[focus].parent));
            } else if (e.key === "0") zoomTo(0);
          }}
        />
        {n && hover && (
          <div
            role="tooltip"
            className="pointer-events-none absolute z-10 w-[340px] border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)]/95 px-3 py-2 shadow-xl backdrop-blur-sm"
            style={{
              left: Math.min(Math.max(0, width - 350), hover.x + 14),
              top: hover.y + 18,
            }}
          >
            <div className="mb-1.5 break-all font-mono text-[11.5px] text-[var(--color-fg)]">{n.name}</div>
            <Row k="total" v={`${formatProfileValue(n.total, unit)} · ${pct(n.total)}`} />
            <Row k="self" v={`${formatProfileValue(n.self, unit)} · ${pct(n.self)}`} />
            {costUsdMonth > 0 && (
              <Row k="$/mo" v={`${formatUsd(frameCost(n.total, rootTotal, costUsdMonth), { cents: true })} total · ${formatUsd(frameCost(n.self, rootTotal, costUsdMonth), { cents: true })} self`} tone="var(--color-signal)" />
            )}
            {diff && <DiffRow n={n} rootTotal={rootTotal} baselineTotal={baselineTotal} />}
          </div>
        )}
      </div>
    </div>
  );
}

function Row({ k, v, tone }: { k: string; v: string; tone?: string }) {
  return (
    <div className="flex items-baseline justify-between gap-3 font-mono text-[11px] leading-5">
      <span className="text-[var(--color-fg-faint)]">{k}</span>
      <span className="tabular-nums" style={{ color: tone ?? "var(--color-fg-dim)" }}>
        {v}
      </span>
    </div>
  );
}

function DiffRow({ n, rootTotal, baselineTotal }: { n: FlameNode; rootTotal: number; baselineTotal: number }) {
  const rel = relativeChange(n, rootTotal, baselineTotal);
  const d = diffOf(n, rootTotal, baselineTotal);
  const tone = d.tone === "regression" ? "var(--color-danger)" : d.tone === "improvement" ? "var(--color-signal)" : "var(--color-fg-dim)";
  const text = rel === Infinity ? "new since baseline" : `${rel >= 0 ? "+" : ""}${(rel * 100).toFixed(1)}% vs baseline · ${d.delta >= 0 ? "+" : ""}${(d.delta * 100).toFixed(2)}pp of total`;
  return <Row k="diff" v={text} tone={tone} />;
}

/** "github.com/x/y.(*T).Method" → "(*T).Method" for breadcrumbs. */
function shortName(name: string): string {
  const slash = name.lastIndexOf("/");
  const tail = slash >= 0 ? name.slice(slash + 1) : name;
  return tail.length > 40 ? `${tail.slice(0, 39)}…` : tail;
}

function fit(ctx: CanvasRenderingContext2D, cache: Map<string, string>, text: string, max: number): string {
  const bucket = Math.floor(max / 6);
  const key = `${bucket}|${text}`;
  const hit = cache.get(key);
  if (hit !== undefined) return hit;
  let out = text;
  if (ctx.measureText(text).width > max) {
    // Prefer the most specific tail of a qualified name, then ellipsize.
    const short = shortName(text);
    out = short;
    if (ctx.measureText(out).width > max) {
      let lo = 0;
      let hi = short.length;
      while (lo < hi) {
        const mid = (lo + hi + 1) >> 1;
        if (ctx.measureText(`${short.slice(0, mid)}…`).width <= max) lo = mid;
        else hi = mid - 1;
      }
      out = lo > 0 ? `${short.slice(0, lo)}…` : "";
    }
  }
  if (cache.size > 5000) cache.clear();
  cache.set(key, out);
  return out;
}
