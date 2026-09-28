// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Empty states always say what to DO next: a one-line cause, a CTA, and
// (when setup is the fix) the exact snippet to paste.

import Link from "next/link";
import type { ComponentType, ReactNode } from "react";
import { ArrowRight } from "lucide-react";

export function EmptyState({
  icon: Icon,
  title,
  body,
  code,
  cta,
  secondary,
  compact = false,
}: {
  icon?: ComponentType<{ className?: string }>;
  title: string;
  body?: ReactNode;
  code?: string;
  cta?: { href: string; label: string };
  secondary?: { href: string; label: string };
  compact?: boolean;
}) {
  return (
    <div className={`flex flex-col items-center text-center ${compact ? "px-4 py-6" : "px-6 py-12"}`}>
      {Icon && <Icon className="mb-3 h-5 w-5 text-[var(--color-fg-faint)]" aria-hidden />}
      <div className="font-mono text-[12.5px] text-[var(--color-fg)]">{title}</div>
      {body && <div className="mt-1.5 max-w-md text-[12.5px] leading-relaxed text-[var(--color-fg-dim)]">{body}</div>}
      {code && (
        <pre className="mt-3 max-w-full overflow-x-auto border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-3 py-2 text-left font-mono text-[11px] leading-relaxed text-[var(--color-cool)]">
          {code}
        </pre>
      )}
      {(cta || secondary) && (
        <div className="mt-4 flex flex-wrap items-center justify-center gap-3">
          {cta && (
            <Link
              href={cta.href}
              className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
            >
              {cta.label}
              <ArrowRight className="h-3 w-3" aria-hidden />
            </Link>
          )}
          {secondary && (
            <Link href={secondary.href} className="font-mono text-[11px] text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              {secondary.label}
            </Link>
          )}
        </div>
      )}
    </div>
  );
}
