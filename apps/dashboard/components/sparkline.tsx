// SPDX-License-Identifier: BUSL-1.1
// Server component — pure SVG, no client JS, no chart library.
//
// Why we wrote our own:
// - recharts / chart.js would add ~150-200KB to First Load JS for what
//   amounts to a polyline. We render maybe 8 of these per page; an SVG
//   primitive is two orders of magnitude lighter.
// - The existing dashboard aesthetic (mono labels, hairline borders)
//   doesn't want chart-library defaults. Better to own the look.
// - Server-renderable, so headers and overview cards aren't blocked
//   on client hydration.

type SparklineProps = {
  values: number[];
  width?: number;
  height?: number;
  color?: string;
  /** Show a faint area fill below the line. Defaults to true. */
  filled?: boolean;
  /** Render an emphasised dot on the last value. Defaults to true. */
  endDot?: boolean;
  className?: string;
  ariaLabel?: string;
};

export function Sparkline({
  values,
  width = 120,
  height = 32,
  color = "var(--color-cool)",
  filled = true,
  endDot = true,
  className,
  ariaLabel,
}: SparklineProps) {
  if (!values || values.length < 2) {
    return (
      <div
        className={className}
        style={{ width, height }}
        aria-hidden
      />
    );
  }

  const pad = 2; // keep the line off the edges so dots/strokes don't clip
  const min = Math.min(...values);
  const max = Math.max(...values);
  const range = max - min || 1;

  const xStep = (width - pad * 2) / (values.length - 1);
  const points = values.map((v, i) => {
    const x = pad + i * xStep;
    const y = height - pad - ((v - min) / range) * (height - pad * 2);
    return [x, y] as const;
  });
  const path = points.map(([x, y], i) => `${i === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`).join(" ");
  const areaPath = filled
    ? `${path} L${(width - pad).toFixed(2)},${(height - pad).toFixed(2)} L${pad.toFixed(2)},${(height - pad).toFixed(2)} Z`
    : "";
  const last = points[points.length - 1];

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      preserveAspectRatio="none"
      role="img"
      aria-label={ariaLabel ?? "trend"}
    >
      {filled && <path d={areaPath} fill={color} fillOpacity={0.08} />}
      <path d={path} fill="none" stroke={color} strokeWidth={1.25} strokeLinecap="round" strokeLinejoin="round" />
      {endDot && (
        <circle cx={last[0]} cy={last[1]} r={2.25} fill={color} />
      )}
    </svg>
  );
}

// Synthetic series generator — deterministic so screenshots stay
// consistent across builds. Used by demo-mode pages until real
// time-series rows ship from the ingest pipeline.
//
//   seed: any string (component name, page id) — same seed → same series
//   length: number of points (default 30)
//   center: target mean
//   variance: ±swing as fraction of center
//   trend: slope as fraction of center per point ("upward 0.5%/day" = 0.005)
export function syntheticSeries({
  seed,
  length = 30,
  center,
  variance = 0.08,
  trend = 0,
}: {
  seed: string;
  length?: number;
  center: number;
  variance?: number;
  trend?: number;
}): number[] {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < seed.length; i++) {
    h ^= seed.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  const next = () => {
    h += 0x6d2b79f5;
    let t = h;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
  const out: number[] = [];
  for (let i = 0; i < length; i++) {
    const noise = (next() - 0.5) * 2 * variance * center;
    const drift = trend * center * i;
    out.push(center + noise + drift);
  }
  return out;
}
