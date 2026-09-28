// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";
import { SkeletonKpi } from "@/components/skeleton";

export default function WorkloadLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "fleet", href: "/fleet" }, { label: "loading…" }]} range={false} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-80 bg-[var(--color-line-bright)]" />
        </div>
        <div className="grid gap-px border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3 lg:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-6 grid gap-4 xl:grid-cols-2">
          {Array.from({ length: 6 }).map((_, i) => (
            <div key={i} className="h-[210px] animate-pulse border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
          ))}
        </div>
      </div>
    </>
  );
}
