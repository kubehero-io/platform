// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ChevronRight, Flame, DollarSign, Cpu, Shield } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { Sparkline, syntheticSeries } from "@/components/sparkline";
import { TableFilter } from "@/components/table-filter";
import { cloudColor, stateMeta, type Cloud } from "@/lib/fleet-data";
import { getFleet } from "@/lib/api/clusters";

export const metadata = { title: "Fleet · KubeHero" };
export const dynamic = "force-dynamic"; // always re-fetch from control-plane

type SearchParams = Promise<{ q?: string; cloud?: string; state?: string }>;

const kpis = [
  { label: "Monthly spend",   value: "$608,240",  sub: "+4.2% vs previous 30d",    icon: DollarSign, tone: "var(--color-warn)",   trend: syntheticSeries({ seed: "fleet-spend",       center: 608, variance: 0.05, trend: 0.0035 }) },
  { label: "Recoverable",     value: "$184,320",  sub: "30.3% of fleet spend",     icon: Flame,      tone: "var(--color-accent)", trend: syntheticSeries({ seed: "fleet-recoverable", center: 184, variance: 0.10, trend: -0.0015 }) },
  { label: "GPU idle share",  value: "41.8%",     sub: "40× A100 · 32× H100",      icon: Cpu,        tone: "var(--color-fg)",     trend: syntheticSeries({ seed: "fleet-gpu-idle",    center: 42,  variance: 0.07 }) },
  { label: "Policies active", value: "12 armed",  sub: "0 firing · cooldown clear", icon: Shield,     tone: "var(--color-signal)", trend: syntheticSeries({ seed: "fleet-armed",       center: 12,  variance: 0.04 }) },
];

export default async function FleetPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const { clusters: all, source } = await getFleet();

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
      <Topbar crumbs={[{ label: "fleet" }]} />
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
          <DataSourceBadge source={source} />
        </div>

        {/* KPIs */}
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          {kpis.map((k) => {
            const Icon = k.icon;
            return (
              <div
                key={k.label}
                className="flex flex-col gap-2 bg-[var(--color-bg-raised)] px-5 py-4"
              >
                <div className="flex items-center justify-between">
                  <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                    {k.label}
                  </span>
                  <Icon className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" />
                </div>
                <div className="flex items-baseline justify-between gap-2">
                  <span
                    className="font-mono text-[22px] tabular-nums tracking-tight"
                    style={{ color: k.tone }}
                  >
                    {k.value}
                  </span>
                  <Sparkline values={k.trend} color={k.tone} width={84} height={24} ariaLabel={`${k.label} trend`} />
                </div>
                <div className="font-mono text-[11px] text-[var(--color-fg-dim)]">
                  {k.sub}
                </div>
              </div>
            );
          })}
        </div>

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
