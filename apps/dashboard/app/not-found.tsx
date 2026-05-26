// SPDX-License-Identifier: BUSL-1.1

import Link from "next/link";
import { Compass, ArrowRight } from "lucide-react";

export default function NotFound() {
  return (
    <main className="grid min-h-screen place-items-center bg-[var(--color-bg)] px-6">
      <div className="w-full max-w-md border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-6">
        <div className="mb-3 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          <Compass className="h-3.5 w-3.5" />
          /// route · 404
        </div>
        <h1 className="mb-2 font-mono text-[20px] tracking-tight text-[var(--color-fg)]">
          Lost in the cluster.
        </h1>
        <p className="mb-5 font-mono text-[12px] text-[var(--color-fg-dim)]">
          That URL does not match any KubeHero view.
        </p>
        <Link
          href="/fleet"
          className="inline-flex items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-1.5 font-mono text-[11px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
        >
          go to fleet
          <ArrowRight className="h-3 w-3" />
        </Link>
      </div>
    </main>
  );
}
