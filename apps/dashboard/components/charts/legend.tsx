// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Chart legend: colour lives in the swatch, text stays in ink tokens.
// Optional click-to-hide for line charts.

export type LegendItem = { key: string; label: string; color: string; value?: string; hidden?: boolean };

export function Legend({
  items,
  onToggle,
  className = "",
}: {
  items: LegendItem[];
  onToggle?: (key: string) => void;
  className?: string;
}) {
  if (items.length < 2) return null;
  return (
    <ul className={`flex flex-wrap items-center gap-x-4 gap-y-1.5 font-mono text-[10.5px] text-[var(--color-fg-dim)] ${className}`}>
      {items.map((it) => {
        const body = (
          <>
            <span
              className="h-2 w-2 shrink-0 rounded-[1px]"
              style={{ background: it.hidden ? "transparent" : it.color, boxShadow: `inset 0 0 0 1px ${it.color}` }}
              aria-hidden
            />
            <span className={`max-w-[200px] truncate ${it.hidden ? "text-[var(--color-fg-faint)] line-through" : ""}`}>{it.label}</span>
            {it.value && <span className="tabular-nums text-[var(--color-fg-faint)]">{it.value}</span>}
          </>
        );
        return (
          <li key={it.key}>
            {onToggle ? (
              <button
                type="button"
                onClick={() => onToggle(it.key)}
                aria-pressed={!it.hidden}
                className="inline-flex items-center gap-1.5 hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
              >
                {body}
              </button>
            ) : (
              <span className="inline-flex items-center gap-1.5">{body}</span>
            )}
          </li>
        );
      })}
    </ul>
  );
}
