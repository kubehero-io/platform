// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /workloads/<cluster>/<namespace>/<name> — the correlation hub. Every
// signal KubeHero has about one workload on one page: what it costs over
// time, its rightsizing recommendation, recent error patterns, its CPU
// hot paths, who it talks to (and what that costs), its alerts, and the
// audit trail — each section deep-linking into the full explorer with
// the query already scoped.

import Link from "next/link";
import { notFound } from "next/navigation";
import { ArrowUpRight, Flame, History, MessageCircleQuestion, PieChart, Scale, ScrollText, Siren, Waypoints } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { Chip, severityTone } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { Sparkline } from "@/components/sparkline";
import { CostChart } from "@/components/cost/cost-chart";
import { UsageDist } from "@/components/charts/usage-dist";
import { getAllocation, getCostTimeseries, listRightsizing } from "@/lib/api/cost";
import { getLogPatterns, getLogVolume } from "@/lib/api/logs";
import { getTopFunctions, listProfileTargets } from "@/lib/api/profiles";
import { getServiceMap } from "@/lib/api/network";
import { listAlerts } from "@/lib/api/alerts";
import { getWorkloadDetail } from "@/lib/api/waste";
import { combineSources } from "@/lib/api/source";
import { LEVEL_COLOR, normalizeLevel } from "@/lib/chart/palette";
import { formatBytes, formatCompact, formatCores, formatGB, formatPct, formatUsd } from "@/lib/chart/scale";
import { edgeClass, EDGE_COLOR } from "@/lib/network/layout";
import { formatProfileValue } from "@/lib/profiles/map";
import { DAY, HOUR, MINUTE } from "@/lib/time-range";
import { logqlSelector, logsHref, workloadHref } from "@/lib/url";
import type { AuditEntry } from "@/lib/api/audit";
import { requestNow } from "@/lib/request-time";

export const dynamic = "force-dynamic";

type Params = Promise<{ slug: string[] }>;

export async function generateMetadata({ params }: { params: Params }) {
  const { slug } = await params;
  const name = slug?.slice(2).map(decodeURIComponent).join("/") || "workload";
  return { title: `${name} · KubeHero` };
}

const NAME_RE = /^[a-zA-Z0-9]([-a-zA-Z0-9._]{0,251}[a-zA-Z0-9])?$/;

function outcomeColor(o: AuditEntry["outcome"]) {
  switch (o) {
    case "applied":
      return "var(--color-signal)";
    case "armed":
      return "var(--color-cool)";
    case "cooldown":
      return "var(--color-warn)";
    case "reverted":
      return "var(--color-fg-dim)";
  }
}

export default async function WorkloadPage({ params }: { params: Params }) {
  const { slug } = await params;
  if (!Array.isArray(slug) || slug.length < 3) notFound();
  const [cluster, namespace, ...nameParts] = slug.map((s) => decodeURIComponent(s));
  const name = nameParts.join("/");
  if (![cluster, namespace, name].every((x) => NAME_RE.test(x))) notFound();

  const now = requestNow();
  const hub = workloadHref(cluster, namespace, name);
  const selector = logqlSelector({ cluster, namespace, workload: name });
  const errSelector = `${selector.slice(0, -1)}, level=~"error|fatal|warn"}`;
  const day = { startMs: now - DAY, endMs: now };
  const hour = { startMs: now - HOUR, endMs: now };
  const filters = { cluster, namespace, workload: name };

  const [alloc, series, rights, patterns, volume, targets, map, alerts, detail] = await Promise.all([
    getAllocation({ window: "30d", aggregate: "workload", filters }),
    getCostTimeseries({ window: "30d", groupBy: "", filters }),
    listRightsizing({ clusterId: cluster, namespace }),
    getLogPatterns({ query: errSelector, range: day, limit: 6 }),
    getLogVolume({ query: selector, range: hour, stepMs: 5 * MINUTE }),
    listProfileTargets({ range: hour, namespace }),
    getServiceMap({ clusterId: cluster, namespace, range: day }),
    listAlerts({ limit: 300 }),
    getWorkloadDetail({ cluster, namespace, name }),
  ]);

  const row = alloc.data.rows.find((r) => (r.properties.workload || r.name) === name) ?? alloc.data.rows[0];
  const monthly = row ? row.totalCost * (730 / (30 * 24)) : 0;
  const recs = rights.data.recs.filter((r) => r.workload === name && r.cluster === cluster);
  const target = targets.data.find((t) => t.workload === name || t.service === name);
  const top = target ? await getTopFunctions({ service: target.service, namespace, type: "cpu", range: hour, limit: 8 }) : null;
  const nodeId = `workload:${namespace}/${name}`;
  const edges = map.data.edges.filter((e) => e.source === nodeId || e.target === nodeId).sort((a, b) => b.costUsdMonth - a.costUsdMonth || b.bytes - a.bytes);
  const nodeName = (id: string) => map.data.nodes.find((n) => n.id === id)?.name ?? id.split("/").pop() ?? id;
  const myAlerts = alerts.data.alerts.filter((a) => a.labels.workload === name && (!a.labels.namespace || a.labels.namespace === namespace) && a.state !== "resolved");
  const lines = volume.data.totalLines;
  const errLines = volume.data.series.filter((s) => ["error", "fatal"].includes(normalizeLevel(s.key))).reduce((a, s) => a + s.values.reduce((x, y) => x + y, 0), 0);
  const savings = recs.reduce((s, r) => s + Math.max(0, r.savingsUsdMonth), 0);
  const src = combineSources(alloc, series, rights, patterns, alerts);
  const knownWorkload = !!row || recs.length > 0 || lines > 0 || !!detail?.rec;
  if (!knownWorkload && src.source === "live") notFound();

  const ask = `/ask?q=${encodeURIComponent(`What's going on with ${namespace}/${name}? Cost, errors, performance.`)}&context=${encodeURIComponent(hub)}`;

  return (
    <>
      <Topbar crumbs={[{ label: "fleet", href: "/fleet" }, { label: cluster, href: `/clusters/${encodeURIComponent(cluster)}` }, { label: `${namespace}/${name}` }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          eyebrow={
            <>
              /// workload · {cluster} · ns {namespace}
              {row?.properties.team ? ` · team ${row.properties.team}` : ""}
              {row?.properties.controllerKind ? ` · ${row.properties.controllerKind}` : ""}
            </>
          }
          title={<span className="font-mono">{name}</span>}
          actions={
            <>
              <Link href={ask} className="btn-secondary !px-2.5 !py-1 !text-[12px]">
                <MessageCircleQuestion className="h-3.5 w-3.5 text-[var(--color-accent)]" aria-hidden /> ask about this workload
              </Link>
              <DataSourceBadge {...src} />
            </>
          }
        />

        <StatGrid cols={5}>
          <StatTile label="Cost / month" value={formatUsd(monthly)} sub={row ? `cpu ${formatUsd(row.cpuCost * (730 / 720))} · ram ${formatUsd(row.ramCost * (730 / 720))}${row.gpuCost > 0 ? ` · gpu ${formatUsd(row.gpuCost * (730 / 720))}` : ""}` : "no allocation yet"} series={series.data.series[0]?.values} seriesColor="var(--color-cool)" href={`/allocation?agg=workload&namespace=${encodeURIComponent(namespace)}&cluster=${encodeURIComponent(cluster)}`} />
          <StatTile label="Efficiency" value={row && row.totalEfficiency > 0 ? formatPct(row.totalEfficiency) : "—"} sub={row ? `cpu ${formatPct(row.cpuEfficiency)} · ram ${formatPct(row.ramEfficiency)}` : "—"} tone={row && row.totalEfficiency < 0.3 ? "var(--color-warn)" : "var(--color-fg)"} />
          <StatTile label="Recoverable" value={savings > 0 ? `${formatUsd(savings)}/mo` : "—"} sub={recs.length ? `${recs.length} container recommendation${recs.length === 1 ? "" : "s"}` : "requests fit usage"} tone="var(--color-signal)" href={`/rightsizing?q=${encodeURIComponent(name)}`} />
          <StatTile label="Errors · 1h" value={lines > 0 ? formatPct(errLines / lines, 1) : "—"} sub={`${formatCompact(errLines)} of ${formatCompact(lines)} lines`} tone={lines > 0 && errLines / lines > 0.05 ? "var(--color-warn)" : "var(--color-fg)"} href={logsHref(errSelector, { window: "1h" })} />
          <StatTile label="Alerts" value={String(myAlerts.length)} sub={myAlerts.length ? myAlerts.map((a) => a.ruleName).join(", ") : "none active"} tone={myAlerts.some((a) => a.severity === "critical") ? "var(--color-accent)" : myAlerts.length ? "var(--color-warn)" : "var(--color-signal)"} href="/alerts" />
        </StatGrid>

        <div className="mt-6 grid gap-4 xl:grid-cols-2">
          <Panel title="/// cost over time · 30d" meta={<Link href={`/allocation?agg=workload&namespace=${encodeURIComponent(namespace)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">allocation →</Link>} bodyClassName="px-4 pb-3 pt-4">
            <CostChart times={series.data.times} stepMs={series.data.stepMs} series={series.data.series} startMs={now - 30 * DAY} endMs={now} height={170} ariaLabel={`${name} daily spend`} />
          </Panel>

          <Panel title="/// rightsizing" meta={<Link href={`/rightsizing?q=${encodeURIComponent(name)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">all recommendations →</Link>} bodyClassName="">
            {recs.length === 0 ? (
              <EmptyState compact icon={Scale} title="No recommendation" body="Requests are within 10% of measured usage (p95 + headroom), or there isn't enough history yet." />
            ) : (
              <ul className="flex flex-col">
                {recs.map((r) => (
                  <li key={r.id} className="border-b border-[var(--color-line)] px-4 py-3 last:border-b-0">
                    <div className="flex flex-wrap items-baseline justify-between gap-2">
                      <Link href={`/rightsizing?rec=${encodeURIComponent(r.id)}`} className="font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]">
                        container {r.container} · {r.replicas}×
                      </Link>
                      <span className="flex items-center gap-2">
                        <Chip tone={r.direction === "upsize" ? "var(--color-warn)" : "var(--color-signal)"}>{r.direction}</Chip>
                        {r.oomKills > 0 && <Chip tone="var(--color-accent)">oom × {r.oomKills}</Chip>}
                        <span className="font-mono text-[12px] tabular-nums" style={{ color: r.savingsUsdMonth >= 0 ? "var(--color-signal)" : "var(--color-warn)" }}>
                          {r.savingsUsdMonth >= 0 ? `${formatUsd(r.savingsUsdMonth)}/mo` : `+${formatUsd(-r.savingsUsdMonth)}/mo`}
                        </span>
                      </span>
                    </div>
                    <div className="mt-2 grid gap-1.5 font-mono text-[10.5px] text-[var(--color-fg-faint)]">
                      <span className="flex items-center gap-3">
                        <span className="w-40">cpu {formatCores(r.cpu.request)} → {formatCores(r.cpu.recommended)}</span>
                        <UsageDist width={200} label="cpu" format={formatCores} p50={r.cpu.p50} p95={r.cpu.p95} p99={r.cpu.p99} max={r.cpu.max} request={r.cpu.request} recommended={r.cpu.recommended} limit={r.cpu.limit} />
                      </span>
                      <span className="flex items-center gap-3">
                        <span className="w-40">mem {formatBytes(r.mem.request)} → {formatBytes(r.mem.recommended)}</span>
                        <UsageDist width={200} label="memory" format={(v) => formatBytes(v)} p50={r.mem.p50} p95={r.mem.p99} p99={r.mem.p99} max={r.mem.max} request={r.mem.request} recommended={r.mem.recommended} limit={r.mem.limit} />
                      </span>
                    </div>
                    <p className="mt-2 text-[12px] leading-relaxed text-[var(--color-fg-dim)]">{r.reason}</p>
                  </li>
                ))}
              </ul>
            )}
          </Panel>

          <Panel title="/// recent error patterns · 24h" meta={<Link href={logsHref(errSelector, { tab: "patterns", window: "24h" })} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">logs →</Link>} bodyClassName="">
            {patterns.data.patterns.length === 0 ? (
              <EmptyState compact icon={ScrollText} title="No warnings or errors in the last 24h" cta={{ href: logsHref(selector, { window: "1h" }), label: "all logs" }} />
            ) : (
              <ul className="flex flex-col">
                {patterns.data.patterns.map((p) => {
                  const lvl = normalizeLevel(p.level);
                  return (
                    <li key={p.pattern} className="flex items-center gap-3 border-b border-[var(--color-line)] px-4 py-2.5 last:border-b-0">
                      <Chip tone={LEVEL_COLOR[lvl]}>{lvl}</Chip>
                      <span className="min-w-0 flex-1 truncate font-mono text-[11.5px] text-[var(--color-fg)]" title={p.sample}>
                        {p.pattern}
                      </span>
                      <Sparkline values={p.trend} width={64} height={18} color={LEVEL_COLOR[lvl]} ariaLabel="pattern trend" />
                      <span className="w-14 shrink-0 text-right font-mono text-[11px] tabular-nums text-[var(--color-fg-dim)]">{formatCompact(p.count)}</span>
                    </li>
                  );
                })}
              </ul>
            )}
          </Panel>

          <Panel title="/// cpu hot paths · 1h" meta={target ? <Link href={`/profiles?service=${encodeURIComponent(target.service)}&namespace=${encodeURIComponent(namespace)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">flamegraph →</Link> : undefined} bodyClassName="">
            {!target || !top || top.data.length === 0 ? (
              <EmptyState compact icon={Flame} title="Not profiled yet" code={`kubectl annotate pod -l app=${name} -n ${namespace} profiles.grafana.com/cpu.scrape=true`} />
            ) : (
              <table className="w-full border-collapse font-mono text-[11.5px]">
                <tbody>
                  {top.data.map((f) => (
                    <tr key={f.name} className="border-b border-[var(--color-line)] last:border-b-0">
                      <td className="max-w-[320px] truncate py-1.5 pl-4 pr-2">
                        <Link href={`/profiles?service=${encodeURIComponent(target.service)}&namespace=${encodeURIComponent(namespace)}&fn=${encodeURIComponent(f.name)}`} className="text-[var(--color-fg)] hover:text-[var(--color-cool)]">
                          {f.name}
                        </Link>
                      </td>
                      <td className="py-1.5 pr-2 text-right tabular-nums text-[var(--color-fg-dim)]">{f.selfPct.toFixed(1)}%</td>
                      <td className="py-1.5 pr-2 text-right tabular-nums text-[var(--color-fg-dim)]">{formatProfileValue(f.self, "nanoseconds")}</td>
                      <td className="py-1.5 pr-4 text-right tabular-nums text-[var(--color-signal)]">{f.selfCostUsdMonth > 0 ? `${formatUsd(f.selfCostUsdMonth)}/mo` : "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="/// network · who it talks to · 24h" meta={<Link href={`/network?cluster=${encodeURIComponent(cluster)}&namespace=${encodeURIComponent(namespace)}`} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">service map →</Link>} bodyClassName="">
            {edges.length === 0 ? (
              <EmptyState compact icon={Waypoints} title="No flows observed" body="Flows come from the collector's eBPF tracer." />
            ) : (
              <table className="w-full border-collapse font-mono text-[11.5px]">
                <tbody>
                  {edges.slice(0, 8).map((e) => {
                    const out = e.source === nodeId;
                    const cls = edgeClass(e);
                    return (
                      <tr key={e.id} className="border-b border-[var(--color-line)] last:border-b-0">
                        <td className="py-1.5 pl-4 pr-2">
                          <span className="inline-block h-2 w-2" style={{ background: EDGE_COLOR[cls] }} aria-hidden />
                        </td>
                        <td className="py-1.5 pr-2 text-[var(--color-fg-faint)]">{out ? "→" : "←"}</td>
                        <td className="max-w-[220px] truncate py-1.5 pr-2 text-[var(--color-fg)]">{nodeName(out ? e.target : e.source)}</td>
                        <td className="py-1.5 pr-2 text-[var(--color-fg-faint)]">:{e.port}</td>
                        <td className="py-1.5 pr-2 text-right tabular-nums text-[var(--color-fg-dim)]">{formatGB(e.bytes / 1e9)}</td>
                        <td className="py-1.5 pr-4 text-right tabular-nums" style={{ color: e.costUsdMonth > 0 ? "var(--color-warn)" : "var(--color-fg-faint)" }}>
                          {e.costUsdMonth > 0 ? `${formatUsd(e.costUsdMonth)}/mo` : "—"}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="/// alerts" meta={<Link href="/alerts" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">all alerts →</Link>} bodyClassName="">
            {myAlerts.length === 0 ? (
              <EmptyState compact icon={Siren} title="No active alerts for this workload" cta={{ href: "/alerts?tab=rules&rule=new", label: "add a rule" }} />
            ) : (
              <ul className="flex flex-col">
                {myAlerts.map((a) => (
                  <li key={a.id} className="border-b border-[var(--color-line)] last:border-b-0">
                    <Link href={a.linkPath || "/alerts"} className="group flex items-start gap-3 px-4 py-2.5 hover:bg-[var(--color-bg-sunken)]/60">
                      <Chip tone={severityTone(a.severity)}>{a.state}</Chip>
                      <span className="min-w-0 flex-1">
                        <span className="block font-mono text-[12px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">{a.ruleName}</span>
                        <span className="block truncate text-[12px] text-[var(--color-fg-dim)]">{a.summary}</span>
                      </span>
                      <ArrowUpRight className="h-3.5 w-3.5 shrink-0 text-[var(--color-fg-faint)]" aria-hidden />
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>

        <Panel className="mt-4" title={<span className="inline-flex items-center gap-2"><History className="h-3 w-3" aria-hidden />/// audit history · {detail?.history.length ?? 0} events</span>} meta={<Link href="/ceilings" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">ceiling log →</Link>} bodyClassName="">
          {!detail || detail.history.length === 0 ? (
            <EmptyState compact title="No policy actions recorded for this workload" />
          ) : (
            <ul className="flex flex-col">
              {detail.history.map((e) => (
                <li key={e.id} className="flex flex-wrap items-center gap-3 border-b border-[var(--color-line)] px-4 py-2.5 font-mono text-[11px] last:border-b-0">
                  <span className="w-40 shrink-0 tabular-nums text-[var(--color-fg-faint)]">{e.at}</span>
                  <span className="w-20 shrink-0 uppercase tracking-[0.14em]" style={{ color: outcomeColor(e.outcome) }}>
                    {e.outcome}
                  </span>
                  <span className="min-w-[200px] flex-1 text-[var(--color-fg-dim)]">{e.action}</span>
                  {e.effectK !== undefined && <span className="shrink-0 tabular-nums text-[var(--color-signal)]">−${e.effectK.toFixed(1)}k/mo</span>}
                  <span className="w-28 shrink-0 text-right text-[var(--color-fg-faint)]">{e.id}</span>
                </li>
              ))}
            </ul>
          )}
        </Panel>

        <div className="mt-4 flex flex-wrap gap-2">
          <HubLink href={logsHref(selector, { window: "1h" })} icon={ScrollText}>logs</HubLink>
          <HubLink href={`/profiles?service=${encodeURIComponent(name)}&namespace=${encodeURIComponent(namespace)}`} icon={Flame}>profiles</HubLink>
          <HubLink href={`/network?cluster=${encodeURIComponent(cluster)}&namespace=${encodeURIComponent(namespace)}`} icon={Waypoints}>network</HubLink>
          <HubLink href={`/allocation?agg=workload&namespace=${encodeURIComponent(namespace)}`} icon={PieChart}>allocation</HubLink>
        </div>
      </div>
    </>
  );
}

function HubLink({ href, icon: Icon, children }: { href: string; icon: React.ComponentType<{ className?: string }>; children: React.ReactNode }) {
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
