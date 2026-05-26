// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";

export default function ClusterLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "fleet", href: "/fleet" }, { label: "loading…" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 h-4 w-24 bg-[var(--color-line-bright)]" />
            <div className="h-6 w-64 bg-[var(--color-line-bright)]" />
          </div>
        </div>
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// top waste · loading
            </span>
          </div>
          {Array.from({ length: 4 }).map((_, i) => (
            <Bar key={i} />
          ))}
        </div>
      </div>
    </>
  );
}

function Bar() {
  return (
    <div className="flex items-center gap-3 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0">
      <div className="h-3 w-8 bg-[var(--color-line-bright)]" />
      <div className="flex-1 space-y-1.5">
        <div className="h-3 w-3/5 bg-[var(--color-line-bright)]" />
        <div className="h-2 w-2/5 bg-[var(--color-line-bright)]" />
      </div>
      <div className="h-6 w-20 bg-[var(--color-line-bright)]" />
    </div>
  );
}
