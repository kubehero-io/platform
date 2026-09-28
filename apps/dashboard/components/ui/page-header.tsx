// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The header every view opens with: a mono section marker, one sentence
// of headline, optional context, and the data-source badge / actions on
// the right. Server component.

import type { ComponentType, ReactNode } from "react";

export function PageHeader({
  eyebrow,
  icon: Icon,
  iconTone,
  title,
  sub,
  actions,
}: {
  /** "/// logs · loki-compatible · last 1h" */
  eyebrow: ReactNode;
  icon?: ComponentType<{ className?: string; style?: React.CSSProperties }>;
  iconTone?: string;
  title: ReactNode;
  sub?: ReactNode;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-6 flex flex-wrap items-end justify-between gap-x-4 gap-y-3">
      <div className="min-w-0 max-w-3xl">
        <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          {Icon && <Icon className="h-3 w-3" style={iconTone ? { color: iconTone } : undefined} aria-hidden />}
          {eyebrow}
        </div>
        <h1 className="text-[22px] font-medium leading-snug tracking-tight text-[var(--color-fg)]">{title}</h1>
        {sub && <p className="mt-2 max-w-2xl text-[13.5px] leading-relaxed text-[var(--color-fg-dim)]">{sub}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
