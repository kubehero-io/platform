// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";

export default function ProfilesLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "profiles" }]} range={{ options: ["15m", "1h", "6h", "24h", "7d"], defaultWindow: "1h", custom: true, refresh: true }} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6 h-14 w-[28rem] max-w-full bg-[var(--color-bg-raised)]" />
        <div className="grid gap-4 xl:grid-cols-[300px_minmax(0,1fr)]">
          <div className="h-[520px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" />
          <div className="flex flex-col gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-4">
            {Array.from({ length: 14 }).map((_, i) => (
              <div key={i} className="h-[19px] animate-pulse bg-[var(--color-line)]" style={{ width: `${100 - i * 5}%`, marginLeft: `${i * 2}%` }} />
            ))}
          </div>
        </div>
      </div>
    </>
  );
}
