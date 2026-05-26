// SPDX-License-Identifier: BUSL-1.1

import Link from "next/link";
import type { ReactNode } from "react";

export function AuthShell({
  title,
  sub,
  footer,
  children,
}: {
  title: string;
  sub: string;
  footer?: ReactNode;
  children: ReactNode;
}) {
  return (
    <main className="relative flex min-h-screen items-center justify-center px-4">
      <div className="absolute inset-0 -z-10 grid-bg" aria-hidden />
      <div className="w-full max-w-[420px]">
        <Link
          href="/"
          className="mb-6 flex items-center gap-2 font-mono text-[11px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]"
        >
          <span className="text-[var(--color-accent)]">▲</span>
          <span>KubeHero</span>
        </Link>
        <div className="bracketed border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-6 md:p-8">
          <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
            {title}
          </h1>
          <p className="mt-2 text-[13px] text-[var(--color-fg-dim)]">{sub}</p>
          <div className="mt-6">{children}</div>
        </div>
        {footer && <div className="mt-5 text-center">{footer}</div>}
      </div>
    </main>
  );
}
