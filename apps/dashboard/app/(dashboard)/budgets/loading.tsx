// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";

export default function BudgetsLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "budgets" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-80 bg-[var(--color-line-bright)]" />
        </div>
        <div className="flex flex-col gap-[1px] border border-[var(--color-line-bright)] bg-[var(--color-line)]">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="bg-[var(--color-bg-raised)] px-4 py-4">
              <div className="mb-2 flex items-center justify-between">
                <div className="h-4 w-48 bg-[var(--color-line-bright)]" />
                <div className="h-4 w-20 bg-[var(--color-line-bright)]" />
              </div>
              <div className="h-2 w-full bg-[var(--color-line-bright)]" />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
