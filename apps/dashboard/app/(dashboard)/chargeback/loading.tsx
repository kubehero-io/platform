// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";
import { SkeletonTable } from "@/components/skeleton";

export default function ChargebackLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "chargeback" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-72 bg-[var(--color-line-bright)]" />
        </div>
        <div className="h-64 w-full border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <table className="w-full min-w-[720px] border-collapse text-[13px]">
            <SkeletonTable rows={5} cols={8} />
          </table>
        </div>
      </div>
    </>
  );
}
