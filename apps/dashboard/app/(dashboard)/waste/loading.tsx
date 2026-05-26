// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";
import { SkeletonTable } from "@/components/skeleton";

export default function WasteLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "waste" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-96 bg-[var(--color-line-bright)]" />
        </div>
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <table className="w-full min-w-[820px] border-collapse text-[13px]">
            <SkeletonTable rows={8} cols={7} />
          </table>
        </div>
      </div>
    </>
  );
}
