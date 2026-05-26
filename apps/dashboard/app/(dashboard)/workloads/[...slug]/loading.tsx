// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";

export default function WorkloadLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "waste", href: "/waste" }, { label: "loading…" }]} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-64 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-80 bg-[var(--color-line-bright)]" />
        </div>
        <div className="mb-6 grid gap-[1px] border border-[var(--color-line-bright)] bg-[var(--color-line)] sm:grid-cols-3">
          {Array.from({ length: 3 }).map((_, i) => (
            <div key={i} className="bg-[var(--color-bg-raised)] px-4 py-3">
              <div className="mb-1 h-2 w-16 bg-[var(--color-line-bright)]" />
              <div className="h-4 w-32 bg-[var(--color-line-bright)]" />
            </div>
          ))}
        </div>
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          {Array.from({ length: 4 }).map((_, i) => (
            <div
              key={i}
              className="flex items-center gap-3 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0"
            >
              <div className="h-3 w-40 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-20 bg-[var(--color-line-bright)]" />
              <div className="h-3 flex-1 bg-[var(--color-line-bright)]" />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
