// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ArrowUpRight, Flame } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { TableFilter } from "@/components/table-filter";
import { AppliedBannerStat, WasteRow } from "@/components/interactive/waste-row";
import { getWaste } from "@/lib/api/waste";

export const metadata = { title: "Waste · KubeHero" };
export const dynamic = "force-dynamic";

type SearchParams = Promise<{ q?: string; cloud?: string; action?: string; window?: string }>;

export default async function WastePage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const win = sp.window === "24h" || sp.window === "7d" ? sp.window : "30d";
  const { rows: all, source } = await getWaste({ limit: 50 });

  const q = (sp.q ?? "").toLowerCase().trim();
  const cloudFilter = (sp.cloud ?? "").toUpperCase();
  const actionFilter = sp.action ?? "";
  const rows = all.filter((r) => {
    if (q && !r.workload.toLowerCase().includes(q) && !r.ns.toLowerCase().includes(q) && !r.cluster.toLowerCase().includes(q)) return false;
    if (cloudFilter && r.cloud !== cloudFilter) return false;
    if (actionFilter && r.action !== actionFilter) return false;
    return true;
  });
  const totalK = rows.reduce((a, r) => a + r.amountK, 0);
  return (
    <>
      <Topbar crumbs={[{ label: "waste" }]} />
      <div className="px-5 py-6">
        <AppliedBannerStat />

        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Flame className="h-3 w-3 text-[var(--color-accent)]" />
              /// top recoverable · {win} · fleet-wide
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Workloads spending more than they use.
            </h1>
          </div>
          <div className="flex items-baseline gap-3 font-mono">
            <div className="flex items-baseline gap-2">
              <span className="text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                total recoverable
              </span>
              <span className="text-[22px] tabular-nums text-[var(--color-accent)]">
                ${totalK.toFixed(1)}k / mo
              </span>
            </div>
            <DataSourceBadge source={source} />
          </div>
        </div>

        <TableFilter
          placeholder="search workload, namespace, or cluster…"
          facets={[
            {
              key: "cloud",
              label: "cloud",
              options: [
                { value: "AKS", label: "AKS" },
                { value: "GKE", label: "GKE" },
                { value: "EKS", label: "EKS" },
              ],
            },
            {
              key: "action",
              label: "action",
              options: [
                { value: "apply", label: "apply" },
                { value: "review", label: "review" },
              ],
            },
          ]}
        />

        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="overflow-x-auto">
            <table className="w-full min-w-[820px] border-collapse text-[13px]">
              <thead>
                <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  <th className="w-8 py-2 pl-4 font-normal">#</th>
                  <th className="py-2 font-normal">Workload</th>
                  <th className="py-2 font-normal">Namespace</th>
                  <th className="py-2 font-normal">Cluster</th>
                  <th className="py-2 font-normal">Signal</th>
                  <th className="py-2 pr-3 text-right font-normal">Recoverable</th>
                  <th className="py-2 pr-4 text-right font-normal">Action</th>
                </tr>
              </thead>
              <tbody>
                {rows.length === 0 && (
                  <tr>
                    <td
                      colSpan={7}
                      className="px-4 py-8 text-center font-mono text-[11px] text-[var(--color-fg-faint)]"
                    >
                      no workloads match this filter ·{" "}
                      <Link href="/waste" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                        clear filter →
                      </Link>
                    </td>
                  </tr>
                )}
                {rows.map((r) => (
                  <WasteRow key={r.rank} r={r} />
                ))}
              </tbody>
            </table>
          </div>
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>sort · recoverable · desc</span>
            <span className="inline-flex items-center gap-1.5 text-[var(--color-cool)]">
              kubehero rightsize --apply
              <ArrowUpRight className="h-3 w-3" />
            </span>
          </div>
        </div>
      </div>
    </>
  );
}
