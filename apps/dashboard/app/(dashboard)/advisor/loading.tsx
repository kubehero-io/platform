// SPDX-License-Identifier: BUSL-1.1

import { Topbar } from "@/components/topbar";

export default function AdvisorLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "agents" }, { label: "advisor" }]} range={false} />
      <div className="px-5 py-6">
        <div className="mb-6">
          <div className="mb-2 h-3 w-72 bg-[var(--color-line-bright)]" />
          <div className="h-6 w-[520px] max-w-full bg-[var(--color-line-bright)]" />
        </div>
        <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
          <div className="h-[480px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
          <div className="h-[480px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
        </div>
      </div>
    </>
  );
}
