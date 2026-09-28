// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /clusters/<id> — one cluster at a glance: spend and idle, efficiency,
// alerts, network, the namespaces that cost the most and the biggest
// rightsizing wins, each linking into the scoped explorer.

import Link from "next/link";
import { notFound } from "next/navigation";
import { ArrowUpRight, Layers, ScrollText, Waypoints } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { Chip } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { EffMeter, ShareBar } from "@/components/charts/meter";
import { cloudColor, stateMeta } from "@/lib/fleet-data";
import { getCluster } from "@/lib/api/clusters";
import { getAllocation, getEfficiency, listRightsizing } from "@/lib/api/cost";
import { listAlerts } from "@/lib/api/alerts";
import { listNetworkCosts } from "@/lib/api/network";
import { combineSources } from "@/lib/api/source";
import { formatCores, formatPct, formatUsd } from "@/lib/chart/scale";
import { DAY } from "@/lib/time-range";
import { logqlSelector, logsHref } from "@/lib/url";
import { requestNow } from "@/lib/request-time";

export const dynamic = "force-dynamic";

export default async function ClusterPage({ params }: { params: Promise<{ id: string }> }) {
  const { id } = await params;
  const { cluster: c, source } = await getCluster(id);
  if (!c) notFound();
  const now = requestNow();
  const [day, week, eff, recs, alerts, network] = await Promise.all([
    getAllocation({ window: "24h", aggregate: "cluster", includeIdle: true, filters: { cluster: c.id } }),
    getAllocation({ window: "7d", aggregate: "namespace", filters: { cluster: c.id } }),
    getEfficiency({ clusterId: c.id }),
    listRightsizing({ clusterId: c.id }),
    listAlerts({ limit: 300 }),
    listNetworkCosts({ clusterId: c.id, range: { startMs: now - DAY, endMs: now } }),
  ]);
  const src = combineSources({ source }, day, week, eff, recs, network);
  const dayTotal = day.data.totals.totalCost;
  const idle = day.data.rows.filter((r) => r.isIdle).reduce((s, r) => s + r.idleCost, 0);
  const clusterEff = eff.data.clusters.find((x) => x.name === c.id) ?? null;
  const firing = alerts.data.alerts.filter((a) => a.state === "firing" && a.labels.cluster === c.id);
  const topNs = week.data.rows.filter((r) => !r.isIdle).slice(0, 10);
  const weekTotal = topNs.reduce((s, r) => s + r.totalCost, 0) || 1;
  const topRecs = recs.data.recs.filter((r) => r.direction !== "ok").slice(0, 8);
  const state = stateMeta[c.state];

  return (
    <>
      <Topbar crumbs={[{ label: "fleet", href: "/fleet" }, { label: c.name }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Layers}
          eyebrow={
            <>
              <span style={{ color: cloudColor[c.cloud] }}>{c.cloud}</span> · {c.region} · {c.nodes} nodes
            </>
          }
          title={<span className="font-mono">{c.name}</span>}
          actions={
            <>
              <span className="inline-flex items-center gap-1.5 font-mono text-[10.5px] uppercase tracking-[0.12em]" style={{ color: state.color }}>
                <span className="h-1.5 w-1.5" style={{ background: state.color }} aria-hidden />
                {state.label}
              </span>
              <DataSourceBadge {...src} />
            </>
          }
        />

        <StatGrid cols={5}>
          <StatTile label="Cost · last 24h" value={formatUsd(dayTotal)} sub={`${formatUsd(dayTotal * 30.4)}/mo run-rate`} href={`/allocation?window=24h&cluster=${encodeURIComponent(c.id)}`} />
          <StatTile label="Idle capacity" value={formatUsd(idle)} sub={dayTotal > 0 ? `${formatPct(idle / dayTotal)} of the day's spend` : "—"} tone={dayTotal > 0 && idle / dayTotal > 0.2 ? "var(--color-warn)" : "var(--color-fg)"} />
          <StatTile label="Efficiency" value={clusterEff ? `${Math.round(clusterEff.score)}/100` : "—"} sub={clusterEff ? `cpu ${formatPct(clusterEff.cpuEfficiency)} · ram ${formatPct(clusterEff.ramEfficiency)}` : "no usage data"} />
          <StatTile label="Alerts firing" value={String(firing.length)} sub={firing.map((a) => a.ruleName).join(", ") || "none"} tone={firing.length ? "var(--color-accent)" : "var(--color-signal)"} href="/alerts" />
          <StatTile label="Network" value={`${formatUsd(network.data.totalUsdMonth)}/mo`} sub="egress + cross-zone" href={`/network?cluster=${encodeURIComponent(c.id)}`} />
        </StatGrid>

        <div className="mt-6 grid gap-4 xl:grid-cols-2">
          <Panel title="/// namespaces by cost · 7d" meta={<Link href={`/allocation?cluster=${encodeURIComponent(c.id)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">allocation →</Link>} bodyClassName="">
            {topNs.length === 0 ? (
              <EmptyState compact title="No allocation data for this cluster yet" />
            ) : (
              <table className="w-full border-collapse text-[12.5px]">
                <tbody>
                  {topNs.map((r) => (
                    <tr key={r.name} className="border-b border-[var(--color-line)] last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50">
                      <td className="py-2 pl-4 pr-3">
                        <Link href={`/allocation?agg=workload&cluster=${encodeURIComponent(c.id)}&namespace=${encodeURIComponent(r.name)}`} className="font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]">
                          {r.name}
                        </Link>
                      </td>
                      <td className="py-2 pr-3">
                        <ShareBar fraction={r.totalCost / weekTotal} color="var(--color-cool)" />
                      </td>
                      <td className="py-2 pr-3">
                        <EffMeter value={r.totalEfficiency} label={`${r.name} efficiency`} />
                      </td>
                      <td className="py-2 pr-4 text-right font-mono tabular-nums text-[var(--color-fg)]">{formatUsd(r.totalCost)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="/// rightsizing · biggest wins" meta={<Link href={`/rightsizing?cluster=${encodeURIComponent(c.id)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">all →</Link>} bodyClassName="">
            {topRecs.length === 0 ? (
              <EmptyState compact title="Requests already fit measured usage" />
            ) : (
              <ul className="flex flex-col">
                {topRecs.map((r) => (
                  <li key={r.id} className="border-b border-[var(--color-line)] last:border-b-0">
                    <Link href={`/rightsizing?cluster=${encodeURIComponent(c.id)}&rec=${encodeURIComponent(r.id)}`} className="group flex items-center gap-3 px-4 py-2.5 hover:bg-[var(--color-bg-sunken)]/60">
                      <span className="min-w-0 flex-1">
                        <span className="block truncate font-mono text-[12.5px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">
                          {r.namespace}/{r.workload}
                        </span>
                        <span className="block font-mono text-[10.5px] text-[var(--color-fg-faint)]">
                          cpu {formatCores(r.cpu.request)} → {formatCores(r.cpu.recommended)} · {r.confidence} confidence
                        </span>
                      </span>
                      {r.oomKills > 0 && <Chip tone="var(--color-accent)">oom</Chip>}
                      <span className="font-mono text-[12px] tabular-nums" style={{ color: r.savingsUsdMonth >= 0 ? "var(--color-signal)" : "var(--color-warn)" }}>
                        {r.savingsUsdMonth >= 0 ? `${formatUsd(r.savingsUsdMonth)}/mo` : `+${formatUsd(-r.savingsUsdMonth)}/mo`}
                      </span>
                      <ArrowUpRight className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" aria-hidden />
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>

        <div className="mt-4 flex flex-wrap gap-2">
          <ClusterLink href={logsHref(`${logqlSelector({ cluster: c.id })}`, { window: "1h" })} icon={ScrollText}>
            logs in this cluster
          </ClusterLink>
          <ClusterLink href={`/network?cluster=${encodeURIComponent(c.id)}`} icon={Waypoints}>
            service map
          </ClusterLink>
        </div>
      </div>
    </>
  );
}

function ClusterLink({ href, icon: Icon, children }: { href: string; icon: React.ComponentType<{ className?: string }>; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
    >
      <Icon className="h-3 w-3" aria-hidden />
      {children}
    </Link>
  );
}
