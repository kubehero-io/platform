// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Stat tile: label · value · sub · optional trend sparkline · optional
// deep link. Tiles sit in a 1px-gap grid (StatGrid) so the hairlines are
// the gaps, not borders. Server component.

import Link from "next/link";
import type { ComponentType, ReactNode } from "react";
import { ArrowUpRight } from "lucide-react";
import { Sparkline } from "@/components/sparkline";

export function StatGrid({ children, cols = 4 }: { children: ReactNode; cols?: 2 | 3 | 4 | 5 | 6 }) {
  const grid = {
    2: "sm:grid-cols-2",
    3: "sm:grid-cols-3",
    4: "sm:grid-cols-2 lg:grid-cols-4",
    5: "sm:grid-cols-3 lg:grid-cols-5",
    6: "sm:grid-cols-3 xl:grid-cols-6",
  }[cols];
  return <div className={`grid gap-px border border-[var(--color-line)] bg-[var(--color-line)] ${grid}`}>{children}</div>;
}

export function StatTile({
  label,
  value,
  sub,
  tone = "var(--color-fg)",
  icon: Icon,
  series,
  seriesColor,
  href,
  hrefLabel,
}: {
  label: string;
  value: ReactNode;
  sub?: ReactNode;
  tone?: string;
  icon?: ComponentType<{ className?: string }>;
  series?: number[];
  seriesColor?: string;
  href?: string;
  hrefLabel?: string;
}) {
  const body = (
    <>
      <div className="flex items-center justify-between gap-2">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">{label}</span>
        {href ? (
          <ArrowUpRight className="h-3.5 w-3.5 text-[var(--color-fg-faint)] transition-colors group-hover:text-[var(--color-cool)]" aria-hidden />
        ) : (
          Icon && <Icon className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" />
        )}
      </div>
      <span className="truncate font-mono text-[22px] leading-tight tracking-tight" style={{ color: tone }} title={typeof value === "string" ? value : undefined}>
        {value}
      </span>
      {series && series.length > 1 && (
        // Full-width trend under the number: tiles stay readable at 6-up.
        <Sparkline values={series} color={seriesColor ?? tone} width={200} height={22} className="h-[22px] w-full" ariaLabel={`${label} trend`} />
      )}
      {sub && <div className="truncate font-mono text-[11px] text-[var(--color-fg-dim)]" title={typeof sub === "string" ? sub : undefined}>{sub}</div>}
    </>
  );
  const cls = "flex min-w-0 flex-col gap-2 bg-[var(--color-bg-raised)] px-5 py-4";
  if (href) {
    return (
      <Link
        href={href}
        aria-label={hrefLabel ?? `${label} — open`}
        className={`group ${cls} transition-colors hover:bg-[var(--color-bg-sunken)]/60 focus-visible:outline focus-visible:outline-1 focus-visible:-outline-offset-1 focus-visible:outline-[var(--color-cool)]`}
      >
        {body}
      </Link>
    );
  }
  return <div className={cls}>{body}</div>;
}
