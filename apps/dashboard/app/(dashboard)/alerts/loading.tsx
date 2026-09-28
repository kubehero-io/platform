// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Topbar } from "@/components/topbar";
import { SkeletonTable } from "@/components/skeleton";

export default function AlertsLoading() {
  return (
    <>
      <Topbar crumbs={[{ label: "control" }, { label: "alerts" }]} range={false} />
      <div className="px-5 py-6" aria-busy="true">
        <div className="mb-6 h-14 w-[26rem] max-w-full bg-[var(--color-bg-raised)]" />
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="h-10 border-b border-[var(--color-line)]" />
          <table className="w-full">
            <SkeletonTable rows={6} cols={5} />
          </table>
        </div>
      </div>
    </>
  );
}
