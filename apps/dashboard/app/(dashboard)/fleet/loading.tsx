// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";
import { SkeletonKpi, SkeletonTable } from "@/components/skeleton";

export default function FleetLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "fleet" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex items-end justify-between gap-3">
          <div>
            <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// fleet · loading
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Every cluster, every cloud.
            </h1>
          </div>
        </div>
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// clusters
            </span>
          </div>
          <table className="w-full min-w-[720px] border-collapse text-[13px]">
            <SkeletonTable rows={6} cols={8} />
          </table>
        </div>
      </div>
    </>
  );
}
