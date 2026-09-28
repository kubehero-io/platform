// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Shared body for per-route error.tsx boundaries: one view failing must
// never take the shell (sidebar, palette) down with it.

import { useEffect } from "react";
import { AlertTriangle, RefreshCcw } from "lucide-react";

export function RouteError({
  view,
  error,
  reset,
}: {
  view: string;
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error(`[dashboard] ${view} render failed`, error);
  }, [view, error]);

  return (
    <div className="px-5 py-12">
      <div role="alert" className="mx-auto max-w-xl border border-[var(--color-warn)]/40 bg-[var(--color-bg-raised)] p-6">
        <div className="mb-3 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-warn)]">
          <AlertTriangle className="h-3.5 w-3.5" aria-hidden />
          /// {view} · render failed
        </div>
        <h1 className="mb-2 font-mono text-[18px] tracking-tight text-[var(--color-fg)]">This view hit an error.</h1>
        <p className="mb-4 font-mono text-[12px] leading-relaxed text-[var(--color-fg-dim)]">
          Every other view still works — use the sidebar or ⌘K. Retry re-runs the page&apos;s queries.
        </p>
        {error.digest && (
          <code className="mb-4 block break-all border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[11px] text-[var(--color-fg-faint)]">
            digest · {error.digest}
          </code>
        )}
        <button
          type="button"
          onClick={reset}
          className="inline-flex items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-1.5 font-mono text-[11px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
        >
          <RefreshCcw className="h-3 w-3" aria-hidden />
          retry
        </button>
      </div>
    </div>
  );
}
