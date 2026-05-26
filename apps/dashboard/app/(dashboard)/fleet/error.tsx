// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useEffect } from "react";
import { AlertTriangle, RefreshCcw } from "lucide-react";

export default function FleetError({
  error,
  reset,
}: {
  error: Error & { digest?: string };
  reset: () => void;
}) {
  useEffect(() => {
    console.error("[fleet] render failed", error);
  }, [error]);

  return (
    <div className="px-5 py-12">
      <div className="mx-auto max-w-xl border border-[var(--color-warn)]/40 bg-[var(--color-bg-raised)] p-6">
        <div className="mb-3 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-warn)]">
          <AlertTriangle className="h-3.5 w-3.5" />
          /// fleet · render failed
        </div>
        <h1 className="mb-2 font-mono text-[18px] tracking-tight text-[var(--color-fg)]">
          We could not load the fleet view.
        </h1>
        <p className="mb-4 font-mono text-[12px] text-[var(--color-fg-dim)]">
          The control-plane is either unreachable or returned malformed data.
          Falling back to demo data has also failed — usually a code-side issue.
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
          <RefreshCcw className="h-3 w-3" />
          retry
        </button>
      </div>
    </div>
  );
}
