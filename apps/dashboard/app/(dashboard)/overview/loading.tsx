// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";
import { SkeletonKpi } from "@/components/skeleton";

export default function OverviewLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "overview" }]} range={false} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-[32rem] max-w-full bg-[var(--color-line-bright)]" />
        </div>
        <div className="grid gap-px border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3 xl:grid-cols-6">
          {Array.from({ length: 6 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-6 grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(0,1fr)]">
          {Array.from({ length: 2 }).map((_, i) => (
            <div key={i} className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
              {Array.from({ length: 5 }).map((__, j) => (
                <div key={j} className="flex items-center gap-4 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0">
                  <div className="h-3 w-16 bg-[var(--color-line-bright)]" />
                  <div className="h-3 flex-1 bg-[var(--color-line)]" />
                  <div className="h-3 w-14 bg-[var(--color-line-bright)]" />
                </div>
              ))}
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
