// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";

export default function LogsLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "logs" }]} range={{ options: ["15m", "1h", "6h", "24h", "7d"], defaultWindow: "1h", custom: true, refresh: true }} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6 h-14 w-[26rem] max-w-full bg-[var(--color-bg-raised)]" />
        <div className="h-10 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)]" />
        <div className="mt-4 h-[190px] animate-pulse border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
        <div className="mt-4 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          {Array.from({ length: 14 }).map((_, i) => (
            <div key={i} className="flex gap-3 border-b border-[var(--color-line)] px-3 py-[7px] last:border-b-0">
              <div className="h-3 w-24 bg-[var(--color-line-bright)]" />
              <div className="h-3 w-10 bg-[var(--color-line-bright)]" />
              <div className="h-3 flex-1 bg-[var(--color-line)]" />
            </div>
          ))}
        </div>
      </div>
    </>
  );
}
