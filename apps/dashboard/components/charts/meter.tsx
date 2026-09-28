// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Inline efficiency meter (usage ÷ request). The number is always
// printed; the fill colour is a secondary cue (amber below 30%, green at
// or above 60%). Server component.

export function effTone(ratio: number): string {
  if (!Number.isFinite(ratio) || ratio <= 0) return "var(--color-fg-faint)";
  if (ratio < 0.3) return "var(--color-warn)";
  if (ratio < 0.6) return "var(--color-cool)";
  return "var(--color-signal)";
}

export function EffMeter({ value, width = 56, label }: { value: number; width?: number; label?: string }) {
  const pct = Number.isFinite(value) ? Math.max(0, value) : 0;
  const fill = Math.min(1, pct);
  return (
    <span className="inline-flex items-center gap-1.5" title={label ? `${label} ${(pct * 100).toFixed(0)}%` : undefined}>
      <span
        className="relative inline-block h-[5px] overflow-hidden rounded-[1px] bg-[var(--color-line)]"
        style={{ width }}
        role="meter"
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct * 100)}
        aria-label={label ?? "efficiency"}
      >
        <span className="absolute inset-y-0 left-0" style={{ width: `${fill * 100}%`, background: effTone(pct) }} />
      </span>
      <span className="w-9 text-right font-mono text-[11px] tabular-nums text-[var(--color-fg-dim)]">
        {pct > 0 ? `${(pct * 100).toFixed(0)}%` : "—"}
      </span>
    </span>
  );
}

/** Horizontal share bar (fraction of a total). */
export function ShareBar({ fraction, color = "var(--color-fg-faint)", width = 64 }: { fraction: number; color?: string; width?: number }) {
  const f = Math.max(0, Math.min(1, Number.isFinite(fraction) ? fraction : 0));
  return (
    <span className="relative inline-block h-[5px] rounded-[1px] bg-[var(--color-line)]" style={{ width }} aria-hidden>
      <span className="absolute inset-y-0 left-0 rounded-[1px]" style={{ width: `${Math.max(f * 100, f > 0 ? 3 : 0)}%`, background: color }} />
    </span>
  );
}
