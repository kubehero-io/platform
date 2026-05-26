// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";

export default function CeilingsLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "ceiling log" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-96 bg-[var(--color-line-bright)]" />
        </div>
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          {Array.from({ length: 6 }).map((_, i) => (
            <div
              key={i}
              className="flex items-center gap-3 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0"
            >
              <div className="h-3 w-40 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-20 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-48 bg-[var(--color-line-bright)]" />
              <div className="h-3 flex-1 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-28 bg-[var(--color-line-bright)]" />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
