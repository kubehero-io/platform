// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";
import { SkeletonKpi, SkeletonTable } from "@/components/skeleton";

export default function PostureLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "posture" }]} />
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
        <div className="mt-6 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <table className="w-full min-w-[920px] border-collapse text-[13px]">
            <SkeletonTable rows={8} cols={7} />
          </table>
        </div>
      </div>
    </>
  );
}
