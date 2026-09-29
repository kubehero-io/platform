// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// One SVG time chart for the whole dashboard, two forms:
//   stacked  — columns per bucket, series stacked bottom-up (log volume by
//              level, cost per day by namespace). Columns are capped at
//              24px with a 1px surface gap between segments.
//   lines    — 2px lines with null gaps (LogQL metric series, trends).
//
// Shared interaction layer: crosshair/column highlight + tooltip on hover
// (and on ←/→ when the plot is focused), drag-to-zoom brush reporting
// [from, to] in epoch ms, a legend for ≥ 2 series, and an sr-only table.

import { useCallback, useMemo, useRef, useState } from "react";
import { Legend } from "./legend";
import { useWidth } from "./use-width";
import { formatCompact, formatTimeFull, formatTimeTick, linear, niceTicks, timeTicks } from "@/lib/chart/scale";

export type ChartSeries = {
  key: string;
  label: string;
  color: string;
  values: (number | null)[];
};

type Props = {
  mode: "stacked" | "lines";
  /** X positions (ms): bucket starts for stacked, sample times for lines. */
  times: number[];
  /** Bucket width (stacked mode). */
  stepMs?: number;
  startMs: number;
  endMs: number;
  series: ChartSeries[];
  height?: number;
  yFormat?: (v: number) => string;
  /** Tooltip value formatter (defaults to yFormat). */
  valueFormat?: (v: number) => string;
  onBrush?: (fromMs: number, toMs: number) => void;
  /** Called with the bucket start when a column is clicked (stacked mode). */
  onPick?: (timeMs: number) => void;
  legend?: boolean;
  ariaLabel: string;
  emptyLabel?: string;
  /** Show totals in the tooltip (stacked). */
  showTotal?: boolean;
};

const M = { top: 8, right: 12, bottom: 22, left: 52 };

export function TimeChart({
  mode,
  times,
  stepMs = 0,
  startMs,
  endMs,
  series,
  height = 180,
  yFormat = formatCompact,
  valueFormat,
  onBrush,
  onPick,
  legend = true,
  ariaLabel,
  emptyLabel = "no data in this range",
  showTotal = true,
}: Props) {
  const [wrapRef, width] = useWidth<HTMLDivElement>();
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [hover, setHover] = useState<number | null>(null);
  const [drag, setDrag] = useState<{ x0: number; x1: number } | null>(null);
  const plotRef = useRef<SVGRectElement>(null);

  const visible = useMemo(() => series.filter((s) => !hidden.has(s.key)), [series, hidden]);
  const fmtV = valueFormat ?? yFormat;

  const innerW = Math.max(0, width - M.left - M.right);
  const innerH = Math.max(0, height - M.top - M.bottom);
  const x = useMemo(() => linear([startMs, endMs], [0, innerW]), [startMs, endMs, innerW]);

  // Y domain: stacked totals or max of any visible line.
  const yMax = useMemo(() => {
    let m = 0;
    if (mode === "stacked") {
      for (let i = 0; i < times.length; i++) {
        let sum = 0;
        for (const s of visible) sum += Math.max(0, s.values[i] ?? 0);
        m = Math.max(m, sum);
      }
    } else {
      for (const s of visible) for (const v of s.values) if (v !== null && Number.isFinite(v)) m = Math.max(m, v);
    }
    return m;
  }, [mode, times.length, visible]);
  const yt = useMemo(() => niceTicks(0, yMax > 0 ? yMax : 1, 4), [yMax]);
  const y = useMemo(() => linear([0, yt.max], [innerH, 0]), [yt.max, innerH]);
  const xt = useMemo(() => timeTicks(startMs, endMs, Math.max(2, Math.floor(innerW / 110))), [startMs, endMs, innerW]);

  const hasData = times.length > 0 && series.some((s) => s.values.some((v) => (v ?? 0) !== 0));

  // Column geometry for stacked mode.
  const colW = useMemo(() => {
    if (mode !== "stacked" || times.length === 0) return 0;
    const slot = stepMs > 0 ? x(startMs + stepMs) - x(startMs) : innerW / times.length;
    return Math.max(1, Math.min(24, slot - (slot > 4 ? 1 : 0)));
  }, [mode, times.length, stepMs, x, startMs, innerW]);

  const indexAt = useCallback(
    (px: number): number | null => {
      if (times.length === 0) return null;
      const t = startMs + (px / Math.max(1, innerW)) * (endMs - startMs);
      if (mode === "stacked" && stepMs > 0) {
        // Bucket containing t.
        let lo = 0;
        let hi = times.length - 1;
        while (lo < hi) {
          const mid = (lo + hi + 1) >> 1;
          if (times[mid] <= t) lo = mid;
          else hi = mid - 1;
        }
        return t >= times[lo] && t < times[lo] + stepMs ? lo : null;
      }
      // Nearest sample.
      let best = 0;
      let bd = Infinity;
      for (let i = 0; i < times.length; i++) {
        const d = Math.abs(times[i] - t);
        if (d < bd) {
          bd = d;
          best = i;
        }
      }
      return best;
    },
    [times, startMs, endMs, innerW, mode, stepMs],
  );

  const localX = (e: React.MouseEvent) => {
    const r = plotRef.current?.getBoundingClientRect();
    if (!r) return 0;
    return Math.min(innerW, Math.max(0, e.clientX - r.left));
  };

  const onMove = (e: React.MouseEvent) => {
    const px = localX(e);
    if (drag) setDrag({ ...drag, x1: px });
    setHover(indexAt(px));
  };
  const onDown = (e: React.MouseEvent) => {
    if (!onBrush || e.button !== 0) return;
    const px = localX(e);
    setDrag({ x0: px, x1: px });
  };
  const onUp = (e: React.MouseEvent) => {
    if (!drag) return;
    const a = Math.min(drag.x0, drag.x1);
    const b = Math.max(drag.x0, drag.x1);
    setDrag(null);
    if (onBrush && b - a >= 6) {
      const span = endMs - startMs;
      onBrush(Math.round(startMs + (a / innerW) * span), Math.round(startMs + (b / innerW) * span));
      return;
    }
    if (onPick) {
      const i = indexAt(localX(e));
      if (i !== null) onPick(times[i]);
    }
  };
  const onKey = (e: React.KeyboardEvent) => {
    if (times.length === 0) return;
    if (e.key === "ArrowRight") {
      e.preventDefault();
      setHover((h) => Math.min(times.length - 1, (h ?? -1) + 1));
    } else if (e.key === "ArrowLeft") {
      e.preventDefault();
      setHover((h) => Math.max(0, (h ?? times.length) - 1));
    } else if (e.key === "Escape") {
      setHover(null);
    }
  };

  const toggle = (key: string) =>
    setHidden((h) => {
      const n = new Set(h);
      if (n.has(key)) n.delete(key);
      else if (visible.length > 1) n.add(key);
      return n;
    });

  const hoverX =
    hover === null ? null : mode === "stacked" && stepMs > 0 ? x(times[hover]) + (x(times[hover] + stepMs) - x(times[hover])) / 2 : x(times[hover]);

  return (
    <div className="min-w-0">
      <div ref={wrapRef} className="relative w-full select-none" style={{ height }}>
        {width > 0 && (
          <svg
            width={width}
            height={height}
            role="img"
            aria-label={ariaLabel}
            className="block overflow-visible"
          >
            <g transform={`translate(${M.left},${M.top})`}>
              {/* y grid + labels */}
              {yt.ticks.map((t) => (
                <g key={t} transform={`translate(0,${y(t)})`}>
                  <line x1={0} x2={innerW} stroke="var(--color-line)" strokeWidth={1} shapeRendering="crispEdges" />
                  <text x={-8} dy="0.32em" textAnchor="end" className="fill-[var(--color-fg-faint)] font-mono text-[10px] tabular-nums">
                    {yFormat(t)}
                  </text>
                </g>
              ))}
              {/* x labels */}
              {xt.ticks.map((t) => (
                <text
                  key={t}
                  x={x(t)}
                  y={innerH + 15}
                  textAnchor="middle"
                  className="fill-[var(--color-fg-faint)] font-mono text-[10px] tabular-nums"
                >
                  {formatTimeTick(t, xt.step)}
                </text>
              ))}
              <line x1={0} x2={innerW} y1={innerH} y2={innerH} stroke="var(--color-line-bright)" shapeRendering="crispEdges" />

              {/* marks */}
              {mode === "stacked" ? (
                <g>
                  {times.map((t, i) => {
                    const slotW = stepMs > 0 ? x(t + stepMs) - x(t) : colW;
                    const cx = x(t) + (slotW - colW) / 2;
                    let acc = 0;
                    const segs: React.ReactNode[] = [];
                    const vis = visible.filter((s) => (s.values[i] ?? 0) > 0);
                    vis.forEach((s, j) => {
                      const v = Math.max(0, s.values[i] ?? 0);
                      const y0 = y(acc);
                      const y1 = y(acc + v);
                      acc += v;
                      const h = y0 - y1;
                      const gap = j < vis.length - 1 && h > 3 ? 1 : 0;
                      segs.push(
                        <rect
                          key={s.key}
                          x={cx}
                          y={y1 + gap}
                          width={colW}
                          height={Math.max(0.5, h - gap)}
                          fill={s.color}
                          opacity={hover === null || hover === i ? 1 : 0.55}
                          rx={j === vis.length - 1 && colW >= 6 ? 1.5 : 0}
                        />,
                      );
                    });
                    return <g key={t}>{segs}</g>;
                  })}
                </g>
              ) : (
                <g fill="none">
                  {visible.map((s) => (
                    <path
                      key={s.key}
                      d={linePath(times, s.values, x, y)}
                      stroke={s.color}
                      strokeWidth={2}
                      strokeLinejoin="round"
                      strokeLinecap="round"
                    />
                  ))}
                  {hover !== null &&
                    visible.map((s) => {
                      const v = s.values[hover];
                      if (v === null || v === undefined || !Number.isFinite(v)) return null;
                      return (
                        <circle key={s.key} cx={x(times[hover])} cy={y(v)} r={4} fill={s.color} stroke="var(--color-bg-raised)" strokeWidth={2} />
                      );
                    })}
                </g>
              )}

              {hoverX !== null && mode === "lines" && (
                <line x1={hoverX} x2={hoverX} y1={0} y2={innerH} stroke="var(--color-fg-faint)" strokeWidth={1} shapeRendering="crispEdges" />
              )}

              {drag && (
                <rect
                  x={Math.min(drag.x0, drag.x1)}
                  y={0}
                  width={Math.abs(drag.x1 - drag.x0)}
                  height={innerH}
                  fill="var(--color-cool)"
                  opacity={0.12}
                  stroke="var(--color-cool)"
                  strokeOpacity={0.5}
                />
              )}

              {!hasData && (
                <text x={innerW / 2} y={innerH / 2} textAnchor="middle" className="fill-[var(--color-fg-faint)] font-mono text-[11px]">
                  {emptyLabel}
                </text>
              )}

              {/* hit layer */}
              <rect
                ref={plotRef}
                x={0}
                y={0}
                width={innerW}
                height={innerH}
                fill="transparent"
                tabIndex={0}
                aria-label={`${ariaLabel} — use arrow keys to read values${onBrush ? ", drag to zoom" : ""}`}
                className={`outline-none focus-visible:stroke-[var(--color-cool)] ${onBrush ? "cursor-crosshair" : ""}`}
                onMouseMove={onMove}
                onMouseLeave={() => {
                  setHover(null);
                  setDrag(null);
                }}
                onMouseDown={onDown}
                onMouseUp={onUp}
                onKeyDown={onKey}
                onBlur={() => setHover(null)}
              />
            </g>
          </svg>
        )}

        {hover !== null && hoverX !== null && width > 0 && (
          <Tooltip
            left={M.left + hoverX}
            width={width}
            title={formatTimeFull(times[hover])}
            rows={visible
              .map((s) => ({ key: s.key, label: s.label, color: s.color, v: s.values[hover] }))
              .filter((r) => r.v !== null && r.v !== undefined)
              .sort((a, b) => (b.v ?? 0) - (a.v ?? 0))
              .slice(0, 10)
              .map((r) => ({ ...r, text: fmtV(r.v as number) }))}
            total={
              mode === "stacked" && showTotal && visible.length > 1
                ? fmtV(visible.reduce((acc, s) => acc + Math.max(0, s.values[hover] ?? 0), 0))
                : undefined
            }
          />
        )}
      </div>

      {legend && (
        <Legend
          className="mt-2 pl-[52px]"
          items={series.map((s) => ({ key: s.key, label: s.label, color: s.color, hidden: hidden.has(s.key) }))}
          onToggle={series.length > 1 ? toggle : undefined}
        />
      )}

      {/* sr-only on the table itself does not hide it: a table grows to its
          content whatever its width, and the invisible box then widens the
          page (horizontal scroll on phones). The wrapper clips it. */}
      <div className="sr-only">
        <table>
          <caption>{ariaLabel}</caption>
          <thead>
            <tr>
              <th scope="col">time</th>
              {series.map((s) => (
                <th key={s.key} scope="col">
                  {s.label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {times.slice(-60).map((t, k) => {
              const i = times.length > 60 ? times.length - 60 + k : k;
              return (
                <tr key={t}>
                  <th scope="row">{formatTimeFull(t)}</th>
                  {series.map((s) => (
                    <td key={s.key}>{s.values[i] === null || s.values[i] === undefined ? "—" : fmtV(s.values[i] as number)}</td>
                  ))}
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function linePath(times: number[], values: (number | null)[], x: (v: number) => number, y: (v: number) => number): string {
  let d = "";
  let pen = false;
  for (let i = 0; i < times.length; i++) {
    const v = values[i];
    if (v === null || v === undefined || !Number.isFinite(v)) {
      pen = false;
      continue;
    }
    d += `${pen ? "L" : "M"}${x(times[i]).toFixed(1)},${y(v).toFixed(1)}`;
    pen = true;
  }
  return d;
}

function Tooltip({
  left,
  width,
  title,
  rows,
  total,
}: {
  left: number;
  width: number;
  title: string;
  rows: { key: string; label: string; color: string; text: string }[];
  total?: string;
}) {
  const flip = left > width - 220;
  return (
    <div
      role="status"
      aria-live="off"
      className="pointer-events-none absolute top-1 z-10 min-w-[180px] max-w-[280px] border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)]/95 px-2.5 py-2 shadow-xl backdrop-blur-sm"
      style={flip ? { right: width - left + 10 } : { left: left + 10 }}
    >
      <div className="mb-1.5 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">{title}</div>
      {rows.length === 0 ? (
        <div className="font-mono text-[11px] text-[var(--color-fg-faint)]">—</div>
      ) : (
        rows.map((r) => (
          <div key={r.key} className="flex items-center gap-2 font-mono text-[11px] leading-5">
            <span className="h-2 w-2 shrink-0 rounded-[1px]" style={{ background: r.color }} aria-hidden />
            <span className="min-w-0 flex-1 truncate text-[var(--color-fg-dim)]">{r.label}</span>
            <span className="tabular-nums text-[var(--color-fg)]">{r.text}</span>
          </div>
        ))
      )}
      {total && (
        <div className="mt-1 flex items-center justify-between border-t border-[var(--color-line)] pt-1 font-mono text-[11px]">
          <span className="text-[var(--color-fg-faint)]">total</span>
          <span className="tabular-nums text-[var(--color-fg)]">{total}</span>
        </div>
      )}
    </div>
  );
}
