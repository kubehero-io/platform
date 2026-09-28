// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /allocation — the OpenCost-class explorer. Every control is a URL
// param, the server runs GetAllocation + GetCostTimeseries in parallel,
// and rows drill down (cluster → namespace → workload → workload hub).

import Link from "next/link";
import { ChevronRight, Download, FileSpreadsheet, PieChart, X } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { ParamSelect, Segmented } from "@/components/ui/param-controls";
import { SortHeader, Th } from "@/components/ui/sort-header";
import { EmptyState } from "@/components/ui/empty-state";
import { EffMeter, ShareBar } from "@/components/charts/meter";
import { CostChart } from "@/components/cost/cost-chart";
import { LabelAggregate, SharedNamespaces } from "@/components/cost/allocation-controls";
import { getAllocation, getCostTimeseries } from "@/lib/api/cost";
import { combineSources } from "@/lib/api/source";
import { drillTarget } from "@/lib/cost/allocation";
import { parseAggregate, TIMESERIES_GROUPS, type Aggregate, type AllocationRow } from "@/lib/cost/types";
import { formatPct, formatUsd } from "@/lib/chart/scale";
import { OTHER_COLOR, seriesColor } from "@/lib/chart/palette";
import { parseSort, sortRows } from "@/lib/table-sort";
import { COST_WINDOW_LABEL, COST_WINDOWS, isCostWindow, monthlyFactor } from "@/lib/time-range";
import { flatParams, hrefWith, param, workloadHref, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Allocation · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/allocation";
const FILTER_KEYS = ["cluster", "namespace", "team", "cost_center", "node", "nodepool", "zone", "workload"] as const;
const NS_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

const AGG_OPTIONS: { value: string; label: string }[] = [
  { value: "namespace", label: "namespace" },
  { value: "workload", label: "workload" },
  { value: "team", label: "team" },
  { value: "cluster", label: "cluster" },
  { value: "nodepool", label: "nodepool" },
  { value: "node", label: "node" },
  { value: "zone", label: "zone" },
];

const SORT_KEYS = ["name", "cpu", "ram", "gpu", "network", "shared", "idle", "total", "eff", "recoverable"] as const;

export default async function AllocationPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const aggregate: Aggregate = parseAggregate(param(sp, "agg"));
  const window = isCostWindow(param(sp, "window")) ? param(sp, "window") : "7d";
  const idleMode = ["hide", "separate", "weighted", "even"].includes(param(sp, "idle")) ? param(sp, "idle") : "separate";
  const shared = param(sp, "shared")
    .split(",")
    .map((s) => s.trim())
    .filter((s) => NS_RE.test(s))
    .slice(0, 20);
  const filters: Record<string, string> = {};
  for (const k of FILTER_KEYS) {
    const v = param(sp, k, 253);
    if (v) filters[k] = v;
  }
  const lbl = param(sp, "lbl", 300);
  if (/^[^=]{1,120}=.{1,160}$/.test(lbl)) {
    const [k, ...v] = lbl.split("=");
    filters[`label:${k}`] = v.join("=");
  }
  const forceDemo = param(sp, "demo") === "1";

  const groupBy = (TIMESERIES_GROUPS as readonly string[]).includes(aggregate) ? aggregate : "";
  const seriesFilters = Object.fromEntries(
    Object.entries(filters).filter(([k]) => ["cluster", "namespace", "workload", "team", "nodepool"].includes(k)),
  );

  const [alloc, series] = await Promise.all([
    getAllocation({
      window,
      aggregate,
      filters,
      includeIdle: idleMode === "separate",
      shareIdle: idleMode === "weighted" || idleMode === "even" ? idleMode : "",
      sharedNamespaces: shared,
      forceDemo,
    }),
    getCostTimeseries({ window, groupBy, filters: seriesFilters, top: 7, forceDemo }),
  ]);
  const src = combineSources(alloc, series);
  const view = alloc.data;

  const sort = parseSort(sp, SORT_KEYS, { key: "total", dir: "desc" });
  const rows = sortRows(view.rows, sort, {
    name: (r) => r.name,
    cpu: (r) => r.cpuCost,
    ram: (r) => r.ramCost,
    gpu: (r) => r.gpuCost,
    network: (r) => r.networkCost,
    shared: (r) => r.sharedCost,
    idle: (r) => r.idleCost,
    total: (r) => r.totalCost,
    eff: (r) => (r.isIdle ? null : r.totalEfficiency),
    recoverable: (r) => r.recoverableCost,
  });
  const t = view.totals;
  const toMonth = monthlyFactor(view.startMs, view.endMs);
  const showGpu = t.gpuCost > 0;

  // Series colour per row name, so the table shares the chart's legend.
  const colorOf = new Map(series.data.series.map((s, i) => [s.key, seriesColor(i, s.key)]));
  const exportQs = new URLSearchParams({ ...params }).toString();
  const filterEntries = Object.entries(filters);

  return (
    <>
      <Topbar crumbs={[{ label: "cost" }, { label: "allocation" }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={PieChart}
          eyebrow={
            <>
              /// allocation · opencost-compatible · {COST_WINDOW_LABEL[window as keyof typeof COST_WINDOW_LABEL]}
            </>
          }
          title="Every dollar, attributed."
          sub="CPU, memory, GPU, network and storage cost by any dimension — with idle capacity and shared platform namespaces handled explicitly, not hidden."
          actions={
            <>
              <a
                href={`/api/export/allocation?${exportQs}`}
                className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
              >
                <Download className="h-3 w-3" aria-hidden /> csv
              </a>
              <a
                href={`/api/export/focus?${new URLSearchParams({ window, aggregate: aggregate.startsWith("label:") ? "namespace" : aggregate }).toString()}`}
                title="FinOps FOCUS 1.2 export from the control plane"
                className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
              >
                <FileSpreadsheet className="h-3 w-3" aria-hidden /> focus
              </a>
              <DataSourceBadge {...src} />
            </>
          }
        />

        {/* controls */}
        <div className="mb-4 flex flex-wrap items-center gap-2">
          <Segmented
            param="agg"
            label="aggregate by"
            value={aggregate.startsWith("label:") ? "" : aggregate}
            defaultValue="namespace"
            options={AGG_OPTIONS}
            clear={["sort", "dir"]}
          />
          <LabelAggregate current={aggregate} />
          <ParamSelect
            param="window"
            label="window"
            value={window}
            defaultValue="7d"
            options={COST_WINDOWS.map((w) => ({ value: w, label: COST_WINDOW_LABEL[w] }))}
          />
          <ParamSelect
            param="idle"
            label="idle"
            value={idleMode}
            defaultValue="separate"
            options={[
              { value: "separate", label: "separate row" },
              { value: "weighted", label: "share · weighted" },
              { value: "even", label: "share · even" },
              { value: "hide", label: "hide" },
            ]}
          />
          <SharedNamespaces current={shared} />
        </div>

        {/* drill breadcrumbs */}
        {filterEntries.length > 0 && (
          <nav aria-label="Filters" className="mb-4 flex flex-wrap items-center gap-1.5 font-mono text-[11px]">
            <Link href={hrefWith(PATH, params, Object.fromEntries([...FILTER_KEYS.map((k) => [k, null]), ["lbl", null], ["agg", null]]))} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              all
            </Link>
            {filterEntries.map(([k, v]) => (
              <span key={k} className="inline-flex items-center gap-1.5">
                <ChevronRight className="h-3 w-3 text-[var(--color-fg-faint)]" aria-hidden />
                <span className="inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5">
                  <span className="text-[var(--color-fg-faint)]">{k}</span>
                  <span className="text-[var(--color-fg)]">{v}</span>
                  <Link
                    href={hrefWith(PATH, params, k.startsWith("label:") ? { lbl: null } : { [k]: null })}
                    aria-label={`remove ${k} filter`}
                    className="text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]"
                  >
                    <X className="h-3 w-3" aria-hidden />
                  </Link>
                </span>
              </span>
            ))}
          </nav>
        )}

        <StatGrid cols={5}>
          <StatTile label="Total cost" value={formatUsd(t.totalCost)} sub={`${formatUsd(t.totalCost * toMonth)}/mo run-rate`} />
          <StatTile
            label="Idle capacity"
            value={formatUsd(t.idleCost)}
            sub={t.totalCost > 0 ? `${formatPct(t.idleCost / t.totalCost)} of spend · ${idleMode === "hide" ? "hidden" : idleMode}` : "—"}
            tone={t.idleCost / Math.max(1, t.totalCost) > 0.2 ? "var(--color-warn)" : "var(--color-fg)"}
          />
          <StatTile
            label="Shared"
            value={formatUsd(t.sharedCost)}
            sub={shared.length > 0 ? shared.join(", ") : "no shared namespaces"}
          />
          <StatTile
            label="Efficiency"
            value={t.totalEfficiency > 0 ? formatPct(t.totalEfficiency) : "—"}
            sub={`cpu ${formatPct(t.cpuEfficiency)} · ram ${formatPct(t.ramEfficiency)}`}
            tone={t.totalEfficiency < 0.3 ? "var(--color-warn)" : "var(--color-fg)"}
          />
          <StatTile
            label="Recoverable"
            value={`${formatUsd(t.recoverableCost * toMonth)}/mo`}
            sub="from rightsizing + idle GPU"
            tone="var(--color-signal)"
            href="/rightsizing"
            hrefLabel="Open rightsizing"
          />
        </StatGrid>

        <Panel
          className="mt-6"
          title={`/// spend over time · ${groupBy ? `by ${groupBy}` : "total"}`}
          meta={`${formatUsd(series.data.totalUsd)} in window${series.data.forecastMonthUsd > 0 ? ` · forecast ${formatUsd(series.data.forecastMonthUsd)} this month` : ""}`}
          bodyClassName="px-4 pb-3 pt-4"
        >
          <CostChart
            times={series.data.times}
            stepMs={series.data.stepMs}
            series={series.data.series}
            startMs={view.startMs}
            endMs={view.endMs}
            ariaLabel={`Spend per ${series.data.stepMs >= 86_400_000 ? "day" : "hour"}${groupBy ? ` by ${groupBy}` : ""}`}
          />
        </Panel>

        <Panel
          className="mt-6"
          title={`/// by ${aggregate} · ${rows.length} rows`}
          meta={`click a row to drill ${aggregate === "workload" ? "into the workload" : "down"}`}
          footer={
            <>
              <span>kubehero cost allocation --aggregate {aggregate} --window {window}</span>
              <span className="normal-case tracking-normal">
                opencost api · <code className="text-[var(--color-cool)]">/allocation/compute?window={window}&amp;aggregate={aggregate}</code>
              </span>
            </>
          }
        >
          {rows.length === 0 ? (
            <EmptyState
              icon={PieChart}
              title="No cost matches these filters"
              body="Remove a filter, or widen the window — allocation needs at least one scrape of pod cost."
              cta={{ href: PATH, label: "clear filters" }}
            />
          ) : (
            <div className="max-h-[70vh] overflow-auto">
              <table className="w-full min-w-[980px] border-collapse text-[12.5px]" data-nav-list>
                <thead>
                  <tr className="border-b border-[var(--color-line)]">
                    <SortHeader column="name" label={aggregate} active={sort.key === "name"} dir={sort.dir} className="pl-4" />
                    <SortHeader column="cpu" label="cpu" align="right" active={sort.key === "cpu"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="ram" label="ram" align="right" active={sort.key === "ram"} dir={sort.dir} className="pr-3" />
                    {showGpu && <SortHeader column="gpu" label="gpu" align="right" active={sort.key === "gpu"} dir={sort.dir} className="pr-3" />}
                    <SortHeader column="network" label="network" align="right" active={sort.key === "network"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="shared" label="shared" align="right" active={sort.key === "shared"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="idle" label="idle" align="right" active={sort.key === "idle"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="total" label="total" align="right" active={sort.key === "total"} dir={sort.dir} className="pr-3" />
                    <Th className="pr-3">share</Th>
                    <SortHeader column="eff" label="efficiency" active={sort.key === "eff"} dir={sort.dir} className="pr-3" title="usage ÷ request, cost-weighted cpu+ram" />
                    <SortHeader column="recoverable" label="recoverable/mo" align="right" active={sort.key === "recoverable"} dir={sort.dir} className="pr-4" />
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <AllocRow
                      key={r.name}
                      r={r}
                      total={t.totalCost}
                      toMonth={toMonth}
                      showGpu={showGpu}
                      color={colorOf.get(r.name) ?? (groupBy ? OTHER_COLOR : undefined)}
                      href={rowHref(aggregate, r, filters, params)}
                    />
                  ))}
                </tbody>
                <tfoot>
                  <tr className="border-t border-[var(--color-line-bright)] font-mono text-[12px]">
                    <td className="sticky bottom-0 bg-[var(--color-bg-raised)] py-2.5 pl-4 uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">total</td>
                    <Money v={t.cpuCost} sticky />
                    <Money v={t.ramCost} sticky />
                    {showGpu && <Money v={t.gpuCost} sticky />}
                    <Money v={t.networkCost} sticky />
                    <Money v={t.sharedCost} sticky />
                    <Money v={t.idleCost} sticky />
                    <Money v={t.totalCost} sticky strong />
                    <td className="sticky bottom-0 bg-[var(--color-bg-raised)]" />
                    <td className="sticky bottom-0 bg-[var(--color-bg-raised)] py-2.5 pr-3">
                      <EffMeter value={t.totalEfficiency} label="fleet efficiency" />
                    </td>
                    <td className="sticky bottom-0 bg-[var(--color-bg-raised)] py-2.5 pr-4 text-right tabular-nums text-[var(--color-signal)]">
                      {formatUsd(t.recoverableCost * toMonth)}
                    </td>
                  </tr>
                </tfoot>
              </table>
            </div>
          )}
        </Panel>
      </div>
    </>
  );
}

function rowHref(aggregate: Aggregate, r: AllocationRow, filters: Record<string, string>, params: Record<string, string>): string | null {
  const d = drillTarget(aggregate, r, filters);
  if (!d) return null;
  if (d.kind === "workload") return workloadHref(d.cluster, d.namespace, d.workload);
  const patch: Record<string, string | null> = { agg: d.aggregate === "namespace" ? null : d.aggregate, sort: null, dir: null };
  for (const [k, v] of Object.entries(d.filters)) {
    if (k.startsWith("label:")) patch.lbl = `${k.slice(6)}=${v}`;
    else patch[k] = v;
  }
  return hrefWith(PATH, params, patch);
}

function AllocRow({
  r,
  total,
  toMonth,
  showGpu,
  color,
  href,
}: {
  r: AllocationRow;
  total: number;
  toMonth: number;
  showGpu: boolean;
  color?: string;
  href: string | null;
}) {
  const label = r.isIdle ? `${r.properties.cluster ? `${r.properties.cluster} · ` : ""}idle capacity` : r.name;
  return (
    <tr className="group border-b border-[var(--color-line)] transition-colors last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50 focus-within:bg-[var(--color-bg-sunken)]/50">
      <td className="max-w-[320px] py-2.5 pl-4 pr-3">
        <span className="flex min-w-0 items-center gap-2">
          <span
            className="h-2 w-2 shrink-0 rounded-[1px]"
            style={{ background: r.isIdle ? "transparent" : (color ?? "transparent"), boxShadow: r.isIdle ? "inset 0 0 0 1px var(--color-fg-faint)" : undefined }}
            aria-hidden
          />
          {href ? (
            <Link href={href} data-nav-item className="min-w-0 truncate font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)] focus-visible:text-[var(--color-cool)] focus-visible:outline-none">
              {label}
            </Link>
          ) : (
            <span className={`min-w-0 truncate font-mono text-[12.5px] ${r.isIdle ? "italic text-[var(--color-fg-dim)]" : "text-[var(--color-fg)]"}`} data-nav-item tabIndex={-1}>
              {label}
            </span>
          )}
          {href && <ChevronRight className="h-3 w-3 shrink-0 text-[var(--color-fg-faint)] opacity-0 transition-opacity group-hover:opacity-100" aria-hidden />}
        </span>
      </td>
      <Money v={r.cpuCost} />
      <Money v={r.ramCost} />
      {showGpu && <Money v={r.gpuCost} />}
      <Money v={r.networkCost} />
      <Money v={r.sharedCost} />
      <Money v={r.idleCost} tone={r.isIdle ? "var(--color-warn)" : undefined} />
      <Money v={r.totalCost} strong />
      <td className="py-2.5 pr-3">
        <span className="inline-flex items-center gap-2">
          <ShareBar fraction={total > 0 ? r.totalCost / total : 0} color={color ?? "var(--color-fg-faint)"} />
          <span className="w-10 text-right font-mono text-[11px] tabular-nums text-[var(--color-fg-faint)]">
            {total > 0 ? formatPct(r.totalCost / total, r.totalCost / total < 0.01 ? 1 : 0) : "—"}
          </span>
        </span>
      </td>
      <td className="py-2.5 pr-3">{r.isIdle ? <span className="font-mono text-[11px] text-[var(--color-fg-faint)]">—</span> : <EffMeter value={r.totalEfficiency} label={`${r.name} efficiency`} />}</td>
      <td className="py-2.5 pr-4 text-right font-mono tabular-nums" style={{ color: r.recoverableCost > 0 ? "var(--color-signal)" : "var(--color-fg-faint)" }}>
        {r.recoverableCost > 0 ? formatUsd(r.recoverableCost * toMonth) : "—"}
      </td>
    </tr>
  );
}

function Money({ v, strong, tone, sticky }: { v: number; strong?: boolean; tone?: string; sticky?: boolean }) {
  return (
    <td
      className={`py-2.5 pr-3 text-right font-mono tabular-nums ${sticky ? "sticky bottom-0 bg-[var(--color-bg-raised)]" : ""} ${strong ? "text-[var(--color-fg)]" : "text-[var(--color-fg-dim)]"}`}
      style={tone && v > 0 ? { color: tone } : v === 0 ? { color: "var(--color-fg-faint)" } : undefined}
    >
      {v === 0 ? "—" : formatUsd(v, { cents: true })}
    </td>
  );
}
