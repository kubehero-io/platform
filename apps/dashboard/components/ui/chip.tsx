// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Mono chip for enums (severity, origin, confidence, direction…). The dot
// carries the colour; the text stays readable ink so meaning never rests
// on colour alone.

import type { ReactNode } from "react";

export function Chip({
  children,
  tone = "var(--color-fg-dim)",
  dot = true,
  title,
  className = "",
}: {
  children: ReactNode;
  tone?: string;
  dot?: boolean;
  title?: string;
  className?: string;
}) {
  return (
    <span
      title={title}
      className={`inline-flex items-center gap-1.5 whitespace-nowrap border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-dim)] ${className}`}
    >
      {dot && <span className="h-1.5 w-1.5 shrink-0" style={{ background: tone }} aria-hidden />}
      {children}
    </span>
  );
}

export const SEVERITY_TONE: Record<string, string> = {
  critical: "var(--color-accent)",
  warn: "var(--color-warn)",
  warning: "var(--color-warn)",
  info: "var(--color-cool)",
};

export function severityTone(s: string | undefined): string {
  return SEVERITY_TONE[(s ?? "").toLowerCase()] ?? "var(--color-fg-faint)";
}
