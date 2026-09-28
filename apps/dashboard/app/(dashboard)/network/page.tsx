// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /network — eBPF service map with priced edges, plus the per-workload
// network cost table (egress + cross-zone).

import Link from "next/link";
import { Globe, Network, Route, Waypoints } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { ParamSelect } from "@/components/ui/param-controls";
import { SortHeader } from "@/components/ui/sort-header";
import { EmptyState } from "@/components/ui/empty-state";
import { ServiceMap } from "@/components/network/service-map";
import { getServiceMap, listNetworkCosts } from "@/lib/api/network";
import { combineSources } from "@/lib/api/source";
import { getFleet } from "@/lib/api/clusters";
import { formatGB, formatUsd } from "@/lib/chart/scale";
import { capNodes, retransmitHeavy } from "@/lib/network/layout";
import { parseSort, sortRows } from "@/lib/table-sort";
import { resolveRange } from "@/lib/time-range";
import { flatParams, hrefWith, param, workloadHref, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Network · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/network";
const WINDOWS = ["1h", "6h", "24h", "7d"] as const;

export default async function NetworkPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const range = resolveRange(sp, { defaultWindow: "24h", allowed: WINDOWS });
  const forceDemo = param(sp, "demo") === "1";
  const fleet = await getFleet();
  const clusterParam = param(sp, "cluster", 253);
  // Demo maps are per cluster; default to the busiest one there.
  const cluster = clusterParam || (fleet.source === "demo" || forceDemo ? "eks-use1-prod" : "");
  const namespace = param(sp, "namespace", 63);

  const [map, costs] = await Promise.all([
    getServiceMap({ clusterId: cluster || undefined, namespace: namespace || undefined, range, maxNodes: 60, forceDemo }),
    listNetworkCosts({ clusterId: cluster || undefined, range, limit: 200, forceDemo }),
  ]);
  const src = combineSources(map, costs);
  const capped = capNodes(map.data.nodes, map.data.edges, 60);
  const heavy = map.data.edges.filter(retransmitHeavy);
  const namespaces = [...new Set(map.data.nodes.map((n) => n.namespace).filter(Boolean))].sort();
  const topEdge = [...map.data.edges].sort((a, b) => b.costUsdMonth - a.costUsdMonth)[0];
  const nodeName = (id: string) => map.data.nodes.find((n) => n.id === id)?.name ?? id;

  const sort = parseSort(sp, ["total", "egress", "crosszone", "workload"] as const, { key: "total", dir: "desc" });
  const rows = sortRows(costs.data.costs, sort, {
    total: (r) => r.totalUsdMonth,
    egress: (r) => r.egressUsdMonth,
    crosszone: (r) => r.crossZoneUsdMonth,
    workload: (r) => `${r.namespace}/${r.workload}`,
  });

  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "network" }]} range={{ options: WINDOWS, defaultWindow: "24h", custom: true, refresh: true }} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Waypoints}
          eyebrow={<>/// network · ebpf flows · {cluster || "all clusters"} · {range.label}</>}
          title="Who talks to whom — and what it costs."
          sub="Every flow is observed in the kernel and priced: cross-zone transfer and internet egress are real line items on the cloud bill."
          actions={
            <>
              <ParamSelect
                param="cluster"
                label="cluster"
                value={cluster}
                allLabel={fleet.source === "demo" || forceDemo ? undefined : "all"}
                clear={["namespace"]}
                options={fleet.clusters.map((c) => ({ value: c.id, label: c.name }))}
              />
              <ParamSelect param="namespace" label="focus" value={namespace} allLabel="all namespaces" options={namespaces.map((n) => ({ value: n, label: n }))} />
              <DataSourceBadge {...src} />
            </>
          }
        />

        <StatGrid cols={4}>
          <StatTile label="Network spend" value={`${formatUsd(map.data.totalCostUsdMonth)}/mo`} sub="egress + cross-zone, at the current rate" tone="var(--color-warn)" icon={Network} />
          <StatTile label="Cross-zone" value={formatGB(map.data.crossZoneGb)} sub={`in ${range.label}`} icon={Route} />
          <StatTile label="Internet egress" value={formatGB(map.data.egressGb)} sub={`in ${range.label}`} icon={Globe} />
          <StatTile
            label="Costliest flow"
            value={topEdge ? `${formatUsd(topEdge.costUsdMonth)}/mo` : "—"}
            sub={topEdge ? `${nodeName(topEdge.source)} → ${nodeName(topEdge.target)}` : "no priced flows"}
            tone={heavy.length > 0 ? "var(--color-warn)" : "var(--color-fg)"}
          />
        </StatGrid>

        <Panel
          className="mt-6"
          title={`/// service map · ${capped.nodes.length} nodes · ${capped.edges.length} flows`}
          meta={heavy.length > 0 ? `${heavy.length} retransmit-heavy link${heavy.length === 1 ? "" : "s"}` : undefined}
          bodyClassName="p-3"
        >
          {capped.nodes.length === 0 ? (
            <EmptyState
              icon={Waypoints}
              title="No flows observed in this window"
              body="The service map comes from the collector's eBPF flow tracer. It needs a kernel with BTF (5.8+) and the collector running privileged."
              code={"helm upgrade kubehero kubehero/kubehero --set collector.ebpf.enabled=true"}
              cta={{ href: hrefWith(PATH, params, { demo: "1" }), label: "preview with demo data" }}
            />
          ) : (
            <ServiceMap key={`${cluster}|${namespace}`} nodes={capped.nodes} edges={capped.edges} cluster={cluster || undefined} />
          )}
        </Panel>

        <Panel
          className="mt-6"
          title={`/// network costs by workload · ${rows.length}`}
          meta={`${formatUsd(costs.data.totalUsdMonth)}/mo total`}
          footer={<span>kubehero network costs {cluster ? `--cluster ${cluster}` : ""}</span>}
        >
          {rows.length === 0 ? (
            <EmptyState compact title="No priced traffic — everything stays inside one zone" />
          ) : (
            <div className="max-h-[520px] overflow-auto">
              <table className="w-full min-w-[860px] border-collapse text-[12.5px]" data-nav-list>
                <thead>
                  <tr className="border-b border-[var(--color-line)]">
                    <SortHeader column="workload" label="workload" active={sort.key === "workload"} dir={sort.dir} className="pl-4" />
                    <SortHeader column="egress" label="egress $/mo" align="right" active={sort.key === "egress"} dir={sort.dir} className="pr-3" />
                    <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 text-right font-mono text-[10px] font-normal uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">egress</th>
                    <SortHeader column="crosszone" label="cross-zone $/mo" align="right" active={sort.key === "crosszone"} dir={sort.dir} className="pr-3" />
                    <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 text-right font-mono text-[10px] font-normal uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">cross-zone</th>
                    <SortHeader column="total" label="total $/mo" align="right" active={sort.key === "total"} dir={sort.dir} className="pr-3" />
                    <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-4 font-mono text-[10px] font-normal uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">top destination</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <tr key={`${r.namespace}/${r.workload}`} className="border-b border-[var(--color-line)] last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50">
                      <td className="py-2 pl-4 pr-3">
                        {cluster ? (
                          <Link href={workloadHref(cluster, r.namespace, r.workload)} data-nav-item className="font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]">
                            {r.namespace}/{r.workload}
                          </Link>
                        ) : (
                          <span className="font-mono text-[12.5px] text-[var(--color-fg)]">
                            {r.namespace}/{r.workload}
                          </span>
                        )}
                      </td>
                      <td className="py-2 pr-3 text-right font-mono tabular-nums" style={{ color: r.egressUsdMonth > 0 ? "var(--color-accent)" : "var(--color-fg-faint)" }}>
                        {r.egressUsdMonth > 0 ? formatUsd(r.egressUsdMonth, { cents: true }) : "—"}
                      </td>
                      <td className="py-2 pr-3 text-right font-mono tabular-nums text-[var(--color-fg-dim)]">{r.egressGb > 0 ? formatGB(r.egressGb) : "—"}</td>
                      <td className="py-2 pr-3 text-right font-mono tabular-nums" style={{ color: r.crossZoneUsdMonth > 0 ? "var(--color-warn)" : "var(--color-fg-faint)" }}>
                        {r.crossZoneUsdMonth > 0 ? formatUsd(r.crossZoneUsdMonth, { cents: true }) : "—"}
                      </td>
                      <td className="py-2 pr-3 text-right font-mono tabular-nums text-[var(--color-fg-dim)]">{r.crossZoneGb > 0 ? formatGB(r.crossZoneGb) : "—"}</td>
                      <td className="py-2 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">{formatUsd(r.totalUsdMonth, { cents: true })}</td>
                      <td className="max-w-[260px] truncate py-2 pr-4 font-mono text-[11.5px] text-[var(--color-fg-dim)]">{r.topDestination || "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      </div>
    </>
  );
}
