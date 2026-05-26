// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Receipt } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { BudgetRow } from "@/components/interactive/budget-row";
import { getPolicies } from "@/lib/api/policies";

export const metadata = { title: "Budgets · KubeHero" };
export const dynamic = "force-dynamic";

export default async function BudgetsPage() {
  const { rows, source } = await getPolicies();
  return (
    <>
      <Topbar crumbs={[{ label: "budgets" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Receipt className="h-3 w-3" />
              /// budget + ceiling policies · {rows.length} tracked
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Spending intent, as code.
            </h1>
            <p className="mt-2 max-w-lg text-[14px] text-[var(--color-fg-dim)]">
              Each row is a YAML CRD in a cluster. Click{" "}
              <span className="font-mono text-[12.5px] text-[var(--color-cool)]">
                arm policy
              </span>{" "}
              to let the escalation plan act; defaults to observation-only.
            </p>
          </div>
          <DataSourceBadge source={source} />
        </div>

        <div className="flex flex-col gap-[1px] border border-[var(--color-line-bright)] bg-[var(--color-line)]">
          {rows.map((b) => (
            <BudgetRow key={b.name} b={b} />
          ))}
        </div>
      </div>
    </>
  );
}
