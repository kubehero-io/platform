// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ChevronRight } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { TableFilter } from "@/components/table-filter";
import { cloudColor, stateMeta, type Cloud } from "@/lib/fleet-data";
import { getFleet } from "@/lib/api/clusters";
import { getAllocation, getCostTimeseries, getEfficiency } from "@/lib/api/cost";
import { getPolicies } from "@/lib/api/policies";
import { combineSources } from "@/lib/api/source";
import { formatPct, formatUsd } from "@/lib/chart/scale";

export const metadata = { title: "Fleet · KubeHero" };
export const dynamic = "force-dynamic"; // always re-fetch from control-plane

type SearchParams = Promise<{ q?: string; cloud?: string; state?: string }>;


export default async function FleetPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const [{ clusters: all, source }, spend, eff, gpu, policies] = await Promise.all([
    getFleet(),
    getCostTimeseries({ window: "30d", groupBy: "" }),
    getEfficiency({ window: "7d" }),
    getAllocation({ window: "30d", aggregate: "cluster" }),
    getPolicies(),
  ]);
  const src = combineSources({ source }, spend, eff, gpu);
  const daily = spend.data.series[0]?.values ?? [];
  const last30 = daily.slice(-30).reduce((a, b) => a + b, 0);
  const prev = daily.slice(-60, -30).reduce((a, b) => a + b, 0);
  const gpuCost = gpu.data.totals.gpuCost;
  const nearCeiling = policies.rows.filter((p) => p.spentPct >= 70).length;

  const q = (sp.q ?? "").toLowerCase().trim();
  const cloudFilter = (sp.cloud ?? "").toUpperCase();
  const stateFilter = sp.state ?? "";
  const clusters = all.filter((c) => {
    if (q && !c.name.toLowerCase().includes(q) && !c.region.toLowerCase().includes(q)) return false;
    if (cloudFilter && c.cloud !== cloudFilter) return false;
    if (stateFilter && c.state !== stateFilter) return false;
    return true;
  });
  const totalNodes = clusters.reduce((s, c) => s + c.nodes, 0);
  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "fleet" }]} range={false} />
      <div className="px-5 py-6">
        <div className="mb-6 flex items-end justify-between gap-3">
          <div>
            <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// fleet · {clusters.length} clusters · {totalNodes} nodes
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Every cluster, every cloud.
            </h1>
          </div>
          <DataSourceBadge {...src} />
        </div>

        {/* KPIs — all computed from the cost plane, nothing hard-coded */}
        <StatGrid cols={4}>
          <StatTile
            label="Spend · last 30d"
            value={formatUsd(last30)}
            sub={prev > 0 ? `${last30 >= prev ? "+" : "−"}${formatPct(Math.abs(last30 / prev - 1), 1)} vs previous 30d` : `forecast ${formatUsd(spend.data.forecastMonthUsd)} this month`}
            series={daily.slice(-30)}
            seriesColor="var(--color-cool)"
            href="/allocation?window=30d&agg=cluster"
          />
          <StatTile
            label="Recoverable"
            value={`${formatUsd(eff.data.recoverableUsdMonth)}/mo`}
            sub={last30 > 0 ? `${formatPct(eff.data.recoverableUsdMonth / last30, 1)} of fleet spend` : "—"}
            tone="var(--color-signal)"
            href="/rightsizing"
          />
          <StatTile
            label="GPU spend · 30d"
            value={formatUsd(gpuCost)}
            sub={gpu.data.totals.totalCost > 0 ? `${formatPct(gpuCost / gpu.data.totals.totalCost, 1)} of allocated spend` : "—"}
            href="/allocation?window=30d&agg=nodepool"
          />
          <StatTile
            label="Budget policies"
            value={`${policies.rows.length}`}
            sub={nearCeiling > 0 ? `${nearCeiling} at ≥ 70% of ceiling` : "all well under ceiling"}
            tone={nearCeiling > 0 ? "var(--color-warn)" : "var(--color-signal)"}
            href="/budgets"
          />
        </StatGrid>

        {/* filter */}
        <div className="mt-8">
          <TableFilter
            placeholder="search cluster name or region…"
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
                key: "state",
                label: "state",
                options: [
                  { value: "healthy", label: "healthy" },
                  { value: "warn", label: "warn" },
                  { value: "critical", label: "critical" },
                ],
              },
            ]}
          />
        </div>

        {/* cluster table */}
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// clusters · {clusters.length} of {all.length}
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              sort · cost · desc
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[720px] border-collapse text-[13px]">
              <thead>
                <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  <th className="w-[28%] py-2 pl-4 font-normal">Cluster</th>
                  <th className="py-2 font-normal">Cloud</th>
                  <th className="py-2 font-normal">Region</th>
                  <th className="py-2 pr-3 text-right font-normal">Nodes</th>
                  <th className="py-2 font-normal">GPU</th>
                  <th className="py-2 pr-3 text-right font-normal">Cost / day</th>
                  <th className="py-2 pr-3 text-right font-normal">Recoverable</th>
                  <th className="py-2 pr-4 text-right font-normal">State</th>
                </tr>
              </thead>
              <tbody>
                {clusters.length === 0 && (
                  <tr>
                    <td
                      colSpan={8}
                      className="px-4 py-8 text-center font-mono text-[11px] text-[var(--color-fg-faint)]"
                    >
                      no clusters match this filter ·{" "}
                      <Link href="/fleet" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                        clear filter →
                      </Link>
                    </td>
                  </tr>
                )}
                {clusters.map((c) => (
                  <tr
                    key={c.id}
                    className="border-b border-[var(--color-line)] transition-colors last:border-b-0 hover:bg-[var(--color-bg-sunken)]/40"
                  >
                    <td className="py-3 pl-4">
                      <Link
                        href={`/clusters/${c.id}`}
                        className="inline-flex items-center gap-2 font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]"
                      >
                        {c.name}
                        <ChevronRight className="h-3 w-3 text-[var(--color-fg-faint)]" />
                      </Link>
                    </td>
                    <td className="py-3 pr-3">
                      <CloudChip cloud={c.cloud} />
                    </td>
                    <td className="py-3 pr-3 font-mono text-[12px] text-[var(--color-fg-dim)]">
                      {c.region}
                    </td>
                    <td className="py-3 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">
                      {c.nodes}
                    </td>
                    <td className="py-3 pr-3 font-mono text-[12px] text-[var(--color-fg-dim)]">
                      {c.gpu}
                    </td>
                    <td className="py-3 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">
                      {c.costDay}
                    </td>
                    <td className="py-3 pr-3 text-right font-mono tabular-nums" style={{ color: stateMeta[c.state].color }}>
                      {c.recoverable}
                    </td>
                    <td className="py-3 pr-4 text-right">
                      <StateBadge state={c.state} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      </div>
    </>
  );
}

function CloudChip({ cloud }: { cloud: Cloud }) {
  return (
    <span
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
      style={{ color: cloudColor[cloud] }}
    >
      <span
        className="h-1.5 w-1.5"
        style={{ background: cloudColor[cloud] }}
        aria-hidden
      />
      {cloud}
    </span>
  );
}

function StateBadge({ state }: { state: "healthy" | "warn" | "critical" }) {
  const s = stateMeta[state];
  return (
    <span
      className="inline-flex items-center gap-1.5 font-mono text-[10.5px] uppercase tracking-[0.12em]"
      style={{ color: s.color }}
    >
      <span className="h-1.5 w-1.5" style={{ background: s.color }} aria-hidden />
      {s.label}
    </span>
  );
}
