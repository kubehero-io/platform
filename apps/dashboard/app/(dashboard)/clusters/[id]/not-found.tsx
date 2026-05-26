// SPDX-License-Identifier: BUSL-1.1

import Link from "next/link";
import { ArrowLeft, Compass } from "lucide-react";
import { Topbar } from "@/components/topbar";

export default function ClusterNotFound() {
  return (
    <>
      <Topbar crumbs={[{ label: "fleet", href: "/fleet" }, { label: "not found" }]} />
      <div className="px-5 py-12">
        <div className="mx-auto max-w-xl border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-6">
          <div className="mb-3 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <Compass className="h-3.5 w-3.5" />
            /// cluster · 404
          </div>
          <h1 className="mb-2 font-mono text-[18px] tracking-tight text-[var(--color-fg)]">
            Cluster not found.
          </h1>
          <p className="mb-4 font-mono text-[12px] text-[var(--color-fg-dim)]">
            The cluster id in the URL does not match any cluster the
            control-plane knows about, and is not part of the demo set.
            It may have been removed or renamed.
          </p>
          <Link
            href="/fleet"
            className="inline-flex items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-1.5 font-mono text-[11px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
          >
            <ArrowLeft className="h-3 w-3" />
            back to fleet
          </Link>
        </div>
      </div>
    </>
  );
}
