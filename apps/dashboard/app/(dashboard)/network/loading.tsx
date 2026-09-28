// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";
import { SkeletonKpi } from "@/components/skeleton";

export default function NetworkLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "network" }]} range={{ options: ["1h", "6h", "24h", "7d"], defaultWindow: "24h", custom: true, refresh: true }} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6 h-14 w-[28rem] max-w-full bg-[var(--color-bg-raised)]" />
        <div className="grid gap-px border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          {Array.from({ length: 4 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-6 aspect-[1200/620] animate-pulse border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
      </div>
    </>
  );
}
