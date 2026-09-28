// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";
import { SkeletonKpi, SkeletonTable } from "@/components/skeleton";

export default function AllocationLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "cost" }, { label: "allocation" }]} range={false} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6 h-14 w-[28rem] max-w-full bg-[var(--color-bg-raised)]" />
        <div className="grid gap-px border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3 lg:grid-cols-5">
          {Array.from({ length: 5 }).map((_, i) => (
            <SkeletonKpi key={i} />
          ))}
        </div>
        <div className="mt-6 h-[240px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
        <div className="mt-6 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <table className="w-full">
            <SkeletonTable rows={8} cols={9} />
          </table>
        </div>
      </div>
    </>
  );
}
