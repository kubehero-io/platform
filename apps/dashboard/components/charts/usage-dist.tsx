// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Mini usage distribution for one resource, on a shared 0…max scale:
//
//   ░░░░░████▓▓▓▓░░░│░░░░░░░░░┆░░░░
//        p50  p95 p99 max      request (dashed) · recommended (green)
//
// The p50→p95 band is the steady state, p95→p99 the tail, the tick the
// observed max. The dashed line is today's request; the solid green line
// is the recommendation — the gap between them is the waste (or, for an
// upsize, the risk). Server component, pure SVG.

export function UsageDist({
  p50,
  p95,
  p99,
  max,
  request,
  recommended,
  limit = 0,
  width = 132,
  label,
  format,
}: {
  p50: number;
  p95: number;
  p99: number;
  max: number;
  request: number;
  recommended: number;
  limit?: number;
  width?: number;
  label: string;
  format: (v: number) => string;
}) {
  const H = 16;
  const top = Math.max(request, recommended, max, limit > 0 ? Math.min(limit, request * 2.5) : 0, 1e-9) * 1.08;
  const x = (v: number) => Math.max(0, Math.min(width, (v / top) * width));
  const title = `${label}: p50 ${format(p50)} · p95 ${format(p95)} · p99 ${format(p99)} · max ${format(max)} · request ${format(request)} · recommended ${format(recommended)}${limit > 0 ? ` · limit ${format(limit)}` : ""}`;
  return (
    <svg width={width} height={H} viewBox={`0 0 ${width} ${H}`} role="img" aria-label={title} className="block overflow-visible">
      <title>{title}</title>
      <rect x={0} y={7} width={width} height={2} fill="var(--color-line-bright)" />
      <rect x={x(p50)} y={4} width={Math.max(1, x(p95) - x(p50))} height={8} fill="var(--color-cool)" opacity={0.85} rx={1} />
      <rect x={x(p95)} y={6} width={Math.max(0, x(p99) - x(p95))} height={4} fill="var(--color-cool)" opacity={0.45} />
      <rect x={x(max) - 0.5} y={3} width={1.5} height={10} fill="var(--color-fg-dim)" />
      {limit > 0 && limit < top && <rect x={x(limit) - 0.5} y={1} width={1} height={14} fill="var(--color-fg-faint)" />}
      <line x1={x(request)} x2={x(request)} y1={0} y2={H} stroke="var(--color-fg)" strokeWidth={1.25} strokeDasharray="2 2" />
      <line x1={x(recommended)} x2={x(recommended)} y1={0} y2={H} stroke="var(--color-signal)" strokeWidth={2} />
    </svg>
  );
}

export function UsageDistLegend() {
  return (
    <span className="inline-flex flex-wrap items-center gap-3 font-mono text-[10px] normal-case tracking-normal text-[var(--color-fg-faint)]">
      <span className="inline-flex items-center gap-1">
        <span className="inline-block h-2 w-3 rounded-[1px] bg-[var(--color-cool)] opacity-85" aria-hidden /> p50–p95
      </span>
      <span className="inline-flex items-center gap-1">
        <span className="inline-block h-2.5 w-[1.5px] bg-[var(--color-fg-dim)]" aria-hidden /> max
      </span>
      <span className="inline-flex items-center gap-1">
        <span className="inline-block h-2.5 border-l border-dashed border-[var(--color-fg)]" aria-hidden /> request
      </span>
      <span className="inline-flex items-center gap-1">
        <span className="inline-block h-2.5 w-[2px] bg-[var(--color-signal)]" aria-hidden /> recommended
      </span>
    </span>
  );
}
