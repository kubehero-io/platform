// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /overview — the command center: what it costs (real daily spend +
// forecast), how efficiently, what is burning right now (alerts, error
// rate, health signals), what changed (anomalies + alerts, merged), and
// the next best actions. Every card deep-links into the explorer that
// explains it, with the query pre-filled.

import Link from "next/link";
import {
  ArrowRight,
  ArrowUpRight,
  Bell,
  CircleDollarSign,
  Gauge,
  Network,
  ScrollText,
  Siren,
  Sparkles,
  TriangleAlert,
} from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { Chip, severityTone } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { CostChart } from "@/components/cost/cost-chart";
import { getAnomalies } from "@/lib/api/anomalies";
import { listAlerts } from "@/lib/api/alerts";
import { getCapacity } from "@/lib/api/capacity";
import { getCostTimeseries, getEfficiency, listRightsizing } from "@/lib/api/cost";
import { getLogVolume } from "@/lib/api/logs";
import { listNetworkCosts } from "@/lib/api/network";
import { getPolicies } from "@/lib/api/policies";
import { combineSources } from "@/lib/api/source";
import { safeLinkPath } from "@/lib/alerts/rules";
import { normalizeLevel } from "@/lib/chart/palette";
import { formatAgo, formatCompact, formatPct, formatUsd } from "@/lib/chart/scale";
import { costWindowRange, HOUR, MINUTE } from "@/lib/time-range";
import { logsHref, workloadHref } from "@/lib/url";
import { requestNow } from "@/lib/request-time";

export const metadata = { title: "Overview · KubeHero" };
export const dynamic = "force-dynamic";

const ERROR_QUERY = '{level=~"error|fatal"}';

type Change = {
  id: string;
  kind: string;
  severity: string;
  title: string;
  detail: string;
  impact: string;
  impactUsd: number;
  age: string;
  href: string;
};

export default async function OverviewPage() {
  const now = requestNow();
  const lastHour = { startMs: now - HOUR, endMs: now };
  const [total, byNs, eff, alerts, volume, network, anomalies, capacity, recs, policies] = await Promise.all([
    getCostTimeseries({ window: "30d", groupBy: "" }),
    getCostTimeseries({ window: "30d", groupBy: "namespace", top: 6 }),
    getEfficiency({ window: "7d" }),
    listAlerts({ limit: 200 }),
    getLogVolume({ query: '{level=~".+"}', range: lastHour, stepMs: 5 * MINUTE }),
    listNetworkCosts({ range: { startMs: now - 24 * HOUR, endMs: now }, limit: 50 }),
    getAnomalies({ window: "7d", limit: 8 }),
    getCapacity({ limit: 20 }),
    listRightsizing({ minSavingsUsdMonth: 100 }),
    getPolicies(),
  ]);
  const src = combineSources(total, eff, alerts, network, recs, anomalies.source === "demo" ? { source: "demo" as const, reason: "unset" as const } : { source: "live" as const });

  // ── KPIs ──────────────────────────────────────────────────────────────
  const daily = total.data.series[0]?.values ?? [];
  const month = costWindowRange("month", now);
  const mtd = total.data.times.reduce((s, t, i) => (t >= month.startMs ? s + (daily[i] ?? 0) : s), 0);
  const forecast = total.data.forecastMonthUsd || mtd;
  const last7 = daily.slice(-7).reduce((a, b) => a + b, 0);
  const prev7 = daily.slice(-14, -7).reduce((a, b) => a + b, 0);
  const wow = prev7 > 0 ? last7 / prev7 - 1 : 0;

  const lines = volume.data.totalLines;
  const errors = volume.data.series.filter((s) => ["error", "fatal"].includes(normalizeLevel(s.key))).reduce((a, s) => a + s.values.reduce((x, y) => x + y, 0), 0);
  const errorSeries = volume.data.times.map((_, i) =>
    volume.data.series.filter((s) => ["error", "fatal"].includes(normalizeLevel(s.key))).reduce((a, s) => a + (s.values[i] ?? 0), 0),
  );
  const firing = alerts.data.alerts.filter((a) => a.state === "firing");
  const critical = firing.filter((a) => a.severity === "critical").length;

  // ── what changed: alerts + anomalies, one ranking ─────────────────────
  const changes: Change[] = [
    ...firing.map((a) => ({
      id: a.id,
      kind: a.kind === "anomaly" ? "anomaly" : "alert",
      severity: a.severity,
      title: a.ruleName + (a.labels.workload ? ` · ${a.labels.workload}` : a.labels.subject ? ` · ${a.labels.subject}` : ""),
      detail: a.summary || a.description,
      impact: a.kind === "anomaly" ? `${formatUsd(a.value)}/mo` : formatCompact(a.value),
      impactUsd: a.kind === "anomaly" ? a.value : 0,
      age: a.firedAt || a.startedAt ? `${formatAgo(now - Date.parse(a.firedAt || a.startedAt))} ago` : "",
      href: a.linkPath || "/alerts",
    })),
    ...anomalies.anomalies
      .filter((an) => !firing.some((a) => a.labels.anomaly === an.id))
      .map((an) => ({
        id: an.id,
        kind: an.kind,
        severity: an.severity,
        title: an.title,
        detail: an.detail,
        impact: `${formatUsd(an.impactUsdMonth)}/mo`,
        impactUsd: an.impactUsdMonth,
        age: an.deltaPct ? `${an.deltaPct > 0 ? "+" : ""}${an.deltaPct.toFixed(0)}%` : "",
        href: safeLinkPath(an.linkPath) || "/overview",
      })),
  ]
    .sort((a, b) => sevRank(b.severity) - sevRank(a.severity) || b.impactUsd - a.impactUsd)
    .slice(0, 8);

  // ── health signals (what an /events page would show) ─────────────────
  const eventAlerts = alerts.data.alerts.filter((a) => a.kind === "event" && a.state !== "resolved");
  const oomRecs = recs.data.recs.filter((r) => r.oomKills > 0);
  const signals = [
    ...eventAlerts.map((a) => ({
      id: a.id,
      tone: severityTone(a.severity),
      label: a.ruleName,
      detail: `${[a.labels.namespace, a.labels.workload].filter(Boolean).join("/")} · ${a.summary || formatCompact(a.value)}`,
      href: a.labels.workload ? `/rightsizing?q=${encodeURIComponent(a.labels.workload)}` : "/alerts",
    })),
    ...oomRecs
      .filter((r) => !eventAlerts.some((a) => a.labels.workload === r.workload))
      .slice(0, 3)
      .map((r) => ({
        id: r.id,
        tone: "var(--color-accent)",
        label: `OOM-killed × ${r.oomKills}`,
        detail: `${r.namespace}/${r.workload} · memory must go up`,
        href: `/rightsizing?rec=${encodeURIComponent(r.id)}`,
      })),
    ...capacity.demands.slice(0, 3).map((d) => ({
      id: d.id,
      tone: "var(--color-warn)",
      label: `${d.pendingPods} unschedulable`,
      detail: `${d.cluster} · ${d.namespace}/${d.workload} · waiting ${d.oldestPendingAge}`,
      href: "/capacity",
    })),
  ].slice(0, 7);

  // ── next actions: reliability fixes first, then the biggest savings ──
  const actions = [
    ...recs.data.recs
      .filter((r) => r.oomKills > 0)
      .slice(0, 2)
      .map((r) => ({
        id: `oom-${r.id}`,
        tone: "var(--color-accent)",
        title: `Raise memory · ${r.namespace}/${r.workload}`,
        subtitle: `${r.oomKills} OOM kills · ${r.cluster}`,
        impact: "reliability",
        rank: Number.MAX_SAFE_INTEGER,
        href: `/rightsizing?rec=${encodeURIComponent(r.id)}`,
      })),
    ...recs.data.recs
      .filter((r) => r.direction === "downsize")
      .slice(0, 4)
      .map((r) => ({
        id: r.id,
        tone: "var(--color-signal)",
        title: `Rightsize ${r.namespace}/${r.workload}`,
        subtitle: `${r.cluster} · confidence ${r.confidence}`,
        impact: `${formatUsd(r.savingsUsdMonth)}/mo`,
        rank: r.savingsUsdMonth,
        href: `/rightsizing?rec=${encodeURIComponent(r.id)}`,
      })),
    ...policies.rows
      .filter((p) => p.spentPct >= 85)
      .slice(0, 1)
      .map((p) => ({
        id: `policy-${p.name}`,
        tone: "var(--color-warn)",
        title: `Budget at ${p.spentPct}% · ${p.name}`,
        subtitle: `${p.scope} · ceiling ${formatUsd(p.ceilingUSD)}`,
        impact: "review",
        rank: 0,
        href: "/budgets",
      })),
  ]
    .sort((a, b) => b.rank - a.rank)
    .slice(0, 6);

  const networkTotal = network.data.totalUsdMonth;
  const topNet = network.data.costs[0];

  return (
    <>
      <Topbar crumbs={[{ label: "overview" }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Gauge}
          eyebrow={<>/// command center · {new Date(now).toISOString().slice(0, 16).replace("T", " ")} utc</>}
          title={critical > 0 ? `${critical} critical alert${critical === 1 ? "" : "s"} firing — and ${formatUsd(eff.data.recoverableUsdMonth)}/mo on the table.` : `All quiet — ${formatUsd(eff.data.recoverableUsdMonth)}/mo on the table.`}
          sub="What changed, what it costs, what to do next. Every card opens the view that explains it."
          actions={
            <>
              <Link href="/ask" className="btn-secondary !px-2.5 !py-1 !text-[12px]">
                <Sparkles className="h-3.5 w-3.5 text-[var(--color-accent)]" aria-hidden /> ask about today
              </Link>
              <DataSourceBadge {...src} />
            </>
          }
        />

        <StatGrid cols={6}>
          <StatTile
            label="Spend · month"
            value={formatUsd(forecast)}
            sub={`forecast · MTD ${formatUsd(mtd)} · ${wow >= 0 ? "+" : "−"}${formatPct(Math.abs(wow), 1)} w/w`}
            series={daily.slice(-30)}
            seriesColor="var(--color-cool)"
            icon={CircleDollarSign}
            href="/allocation?window=month"
            hrefLabel="Open allocation for this month"
          />
          <StatTile
            label="Recoverable"
            value={`${formatUsd(eff.data.recoverableUsdMonth)}/mo`}
            sub={`${recs.data.recs.filter((r) => r.direction === "downsize").length} rightsizing recs`}
            tone="var(--color-signal)"
            href="/rightsizing"
          />
          <StatTile
            label="Efficiency"
            value={`${Math.round(eff.data.score)}/100`}
            sub={`cpu ${formatPct(eff.data.cpuEfficiency)} · ram ${formatPct(eff.data.ramEfficiency)} · idle ${formatUsd(eff.data.idleCostUsdMonth)}/mo`}
            tone={eff.data.score < 40 ? "var(--color-warn)" : "var(--color-fg)"}
            href="/allocation?idle=separate"
          />
          <StatTile
            label="Alerts firing"
            value={String(firing.length)}
            sub={`${critical} critical · ${alerts.data.pending} pending`}
            tone={critical > 0 ? "var(--color-accent)" : firing.length > 0 ? "var(--color-warn)" : "var(--color-signal)"}
            icon={Siren}
            href="/alerts"
          />
          <StatTile
            label="Log errors · 1h"
            value={`${formatCompact(errors / 60)}/min`}
            sub={lines > 0 ? `${formatPct(errors / lines, 1)} of ${formatCompact(lines)} lines` : "no log lines"}
            tone={lines > 0 && errors / lines > 0.05 ? "var(--color-warn)" : "var(--color-fg)"}
            series={errorSeries}
            seriesColor="var(--color-warn)"
            icon={ScrollText}
            href={logsHref(ERROR_QUERY, { window: "1h" })}
          />
          <StatTile
            label="Network"
            value={`${formatUsd(networkTotal)}/mo`}
            sub={topNet ? `top: ${topNet.workload} ${formatUsd(topNet.totalUsdMonth)}/mo` : "egress + cross-zone"}
            icon={Network}
            href="/network"
          />
        </StatGrid>

        <div className="mt-6 grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(0,1fr)]">
          <Panel title="/// what changed · alerts + anomalies" meta={`${changes.length} items`} bodyClassName="">
            {changes.length === 0 ? (
              <EmptyState compact icon={Bell} title="Nothing changed that's worth your attention" />
            ) : (
              <ul className="flex flex-col" data-nav-list>
                {changes.map((c) => (
                  <li key={c.id} className="border-b border-[var(--color-line)] last:border-b-0">
                    <Link
                      href={c.href}
                      data-nav-item
                      className="group grid grid-cols-[88px_minmax(0,1fr)_auto] items-start gap-3 px-4 py-2.5 transition-colors hover:bg-[var(--color-bg-sunken)]/60 focus-visible:bg-[var(--color-bg-sunken)] focus-visible:outline-none"
                    >
                      <Chip tone={severityTone(c.severity)}>{c.kind}</Chip>
                      <span className="min-w-0">
                        <span className="block truncate font-mono text-[12.5px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">{c.title}</span>
                        <span className="block truncate text-[12px] text-[var(--color-fg-dim)]">{c.detail}</span>
                      </span>
                      <span className="flex flex-col items-end gap-0.5 font-mono text-[11px] tabular-nums">
                        <span className="text-[var(--color-fg)]">{c.impact}</span>
                        <span className="text-[var(--color-fg-faint)]">{c.age}</span>
                      </span>
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>

          <Panel title="/// health signals · oom · unschedulable · crash loops" meta="from events, alerts, rightsizing" bodyClassName="">
            {signals.length === 0 ? (
              <EmptyState compact icon={TriangleAlert} title="No OOM kills, crash loops or unschedulable pods" />
            ) : (
              <ul className="flex flex-col">
                {signals.map((s) => (
                  <li key={s.id} className="border-b border-[var(--color-line)] last:border-b-0">
                    <Link href={s.href} className="group flex items-start gap-3 px-4 py-2.5 transition-colors hover:bg-[var(--color-bg-sunken)]/60">
                      <span className="mt-1.5 h-1.5 w-1.5 shrink-0" style={{ background: s.tone }} aria-hidden />
                      <span className="min-w-0 flex-1">
                        <span className="block font-mono text-[12px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">{s.label}</span>
                        <span className="block truncate text-[12px] text-[var(--color-fg-dim)]">{s.detail}</span>
                      </span>
                      <ArrowUpRight className="mt-0.5 h-3.5 w-3.5 shrink-0 text-[var(--color-fg-faint)] group-hover:text-[var(--color-cool)]" aria-hidden />
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>

        <div className="mt-6 grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(0,1fr)]">
          <Panel
            title="/// daily spend · 30d · by namespace"
            meta={<Link href="/allocation" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">open allocation →</Link>}
            bodyClassName="px-4 pb-3 pt-4"
          >
            <CostChart
              times={byNs.data.times}
              stepMs={byNs.data.stepMs}
              series={byNs.data.series}
              startMs={now - 30 * 24 * HOUR}
              endMs={now}
              height={190}
              ariaLabel="Daily spend by namespace, last 30 days"
            />
          </Panel>

          <Panel title="/// next best actions · reliability, then $/mo" meta={`${actions.length}`} bodyClassName="">
            {actions.length === 0 ? (
              <EmptyState compact title="Nothing actionable right now" cta={{ href: "/rightsizing", label: "review rightsizing" }} />
            ) : (
              <ul className="flex flex-col">
                {actions.map((a, i) => (
                  <li key={a.id} className="border-b border-[var(--color-line)] last:border-b-0">
                    <Link href={a.href} className="group flex items-center gap-3 px-4 py-2.5 transition-colors hover:bg-[var(--color-bg-sunken)]/60">
                      <span className="w-5 shrink-0 font-mono text-[10px] tabular-nums text-[var(--color-fg-faint)]">{String(i + 1).padStart(2, "0")}</span>
                      <span className="min-w-0 flex-1">
                        <span className="block truncate font-mono text-[12.5px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">{a.title}</span>
                        <span className="block truncate font-mono text-[10.5px] text-[var(--color-fg-dim)]">{a.subtitle}</span>
                      </span>
                      <span className="shrink-0 font-mono text-[12px] tabular-nums" style={{ color: a.tone }}>
                        {a.impact}
                      </span>
                      <ArrowRight className="h-3 w-3 shrink-0 text-[var(--color-fg-faint)] group-hover:text-[var(--color-cool)]" aria-hidden />
                    </Link>
                  </li>
                ))}
              </ul>
            )}
          </Panel>
        </div>

        <Forecast
          forecast={forecast}
          recoverable={recs.data.totalSavingsUsdMonth}
          gpuIdle={eff.data.recoverableUsdMonth - recs.data.totalSavingsUsdMonth}
          topWorkload={recs.data.recs[0] ? workloadHref(recs.data.recs[0].cluster, recs.data.recs[0].namespace, recs.data.recs[0].workload) : "/rightsizing"}
        />
      </div>
    </>
  );
}

function Forecast({ forecast, recoverable, gpuIdle, topWorkload }: { forecast: number; recoverable: number; gpuIdle: number; topWorkload: string }) {
  const rows = [
    { label: "This month at the current trajectory", value: forecast, delta: "linear projection · MTD + trailing 7-day daily average", href: "/allocation?window=month", cta: "breakdown", tone: "var(--color-warn)" },
    { label: "With every rightsizing recommendation applied", value: Math.max(0, forecast - recoverable), delta: `−${formatUsd(recoverable)}/mo`, href: "/rightsizing", cta: "review", tone: "var(--color-cool)" },
    { label: "…and idle GPUs capped by a ceiling", value: Math.max(0, forecast - recoverable - Math.max(0, gpuIdle)), delta: `−${formatUsd(Math.max(0, gpuIdle))}/mo more`, href: topWorkload, cta: "inspect", tone: "var(--color-signal)" },
  ];
  return (
    <Panel className="mt-6" title="/// month-end forecast · with verbs" meta="guarded changes only — humans arm, the operator applies">
      {rows.map((r) => (
        <div key={r.label} className="flex flex-wrap items-baseline gap-3 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0">
          <span className="min-w-[280px] font-mono text-[12px] text-[var(--color-fg-dim)]">{r.label}</span>
          <span className="font-mono text-[18px] tabular-nums" style={{ color: r.tone }}>
            {formatUsd(r.value)}
          </span>
          <span className="font-mono text-[11px] text-[var(--color-fg-faint)]">{r.delta}</span>
          <Link
            href={r.href}
            className="ml-auto inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
          >
            {r.cta} <ArrowRight className="h-3 w-3" aria-hidden />
          </Link>
        </div>
      ))}
    </Panel>
  );
}

function sevRank(s: string): number {
  return s === "critical" ? 3 : s === "warn" ? 2 : 1;
}
