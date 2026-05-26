// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";
import { SkeletonKpi } from "@/components/skeleton";

export default function OverviewLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "overview" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-96 bg-[var(--color-line-bright)]" />
        </div>
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          {Array.from({ length: 5 }).map((_, i) => (
            <div
              key={i}
              className="flex items-center gap-4 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0"
            >
              <div className="h-3 w-8 bg-[var(--color-line-bright)]" />
              <div className="h-3 flex-1 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-20 bg-[var(--color-line-bright)]" />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
