// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /rightsizing — VPA-class, percentile-based recommendations per
// container (ListRightsizing). The page only ever PROPOSES: each row's
// drawer generates a guarded RightsizingPolicy for the operator's arming
// flow. (The old /waste list lives here now.)

import Link from "next/link";
import { AlertOctagon, ArrowDownRight, ArrowUpRight, Gauge, MemoryStick, Scale } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { StatGrid, StatTile } from "@/components/ui/stat-tile";
import { ParamSelect, Segmented } from "@/components/ui/param-controls";
import { SortHeader, Th } from "@/components/ui/sort-header";
import { Chip } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { UrlDrawer } from "@/components/ui/drawer";
import { TableFilter } from "@/components/table-filter";
import { UsageDist, UsageDistLegend } from "@/components/charts/usage-dist";
import { PolicyPanel } from "@/components/rightsizing/policy-panel";
import { getAllocation, listRightsizing } from "@/lib/api/cost";
import type { Rightsizing } from "@/lib/cost/types";
import { formatBytes, formatCores, formatPct, formatUsd } from "@/lib/chart/scale";
import { matchesQuery, parseSort, sortRows } from "@/lib/table-sort";
import { flatParams, hrefWith, logqlSelector, logsHref, param, workloadHref, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Rightsizing · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/rightsizing";
const CONF_RANK = { low: 0, medium: 1, high: 2 } as const;
const SORTS = ["savings", "workload", "cpu", "mem", "confidence"] as const;

export default async function RightsizingPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const window = ["24h", "7d", "30d"].includes(param(sp, "window")) ? param(sp, "window") : "7d";
  const pct = (["p90", "p95", "p99", "max"].includes(param(sp, "pct")) ? param(sp, "pct") : "p95") as "p90" | "p95" | "p99" | "max";
  const headroomRaw = Number(param(sp, "headroom") || "15");
  const headroom = [0, 10, 15, 25, 50].includes(headroomRaw) ? headroomRaw : 15;
  const dirFilter = ["downsize", "upsize"].includes(param(sp, "direction")) ? param(sp, "direction") : "";
  const confFilter = ["medium", "high"].includes(param(sp, "conf")) ? (param(sp, "conf") as "medium" | "high") : "";
  const cluster = param(sp, "cluster", 253);
  const namespace = param(sp, "namespace", 63);
  const q = param(sp, "q", 200);
  const recId = param(sp, "rec", 600);

  const res = await listRightsizing({
    window,
    cpuPercentile: pct,
    headroomPct: headroom,
    clusterId: cluster || undefined,
    namespace: namespace || undefined,
    forceDemo: param(sp, "demo") === "1",
  });
  const all = res.data.recs;
  const filtered = all.filter((r) => {
    if (dirFilter && r.direction !== dirFilter) return false;
    if (confFilter && CONF_RANK[r.confidence] < CONF_RANK[confFilter]) return false;
    return matchesQuery(q, r.workload, r.namespace, r.cluster, r.container);
  });
  const sort = parseSort(sp, SORTS, { key: "savings", dir: "desc" });
  const rows = sortRows(filtered, sort, {
    savings: (r) => r.savingsUsdMonth,
    workload: (r) => `${r.namespace}/${r.workload}`,
    cpu: (r) => (r.cpu.request - r.cpu.recommended) * r.replicas,
    mem: (r) => (r.mem.request - r.mem.recommended) * r.replicas,
    confidence: (r) => CONF_RANK[r.confidence],
  });

  const downs = filtered.filter((r) => r.direction === "downsize");
  const ups = filtered.filter((r) => r.direction === "upsize");
  const savings = downs.reduce((s, r) => s + Math.max(0, r.savingsUsdMonth), 0);
  const cores = downs.reduce((s, r) => s + Math.max(0, r.cpu.request - r.cpu.recommended) * r.replicas, 0);
  const bytes = downs.reduce((s, r) => s + Math.max(0, r.mem.request - r.mem.recommended) * r.replicas, 0);
  const ooms = filtered.filter((r) => r.oomKills > 0);

  const clusters = [...new Set(all.map((r) => r.cluster))].sort();
  const namespaces = [...new Set(all.filter((r) => !cluster || r.cluster === cluster).map((r) => r.namespace))].sort();

  const selected = recId ? all.find((r) => r.id === recId) : undefined;
  const siblings = selected ? await siblingWorkloads(selected, res.source === "demo") : [];

  return (
    <>
      <Topbar crumbs={[{ label: "cost" }, { label: "rightsizing" }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Scale}
          eyebrow={
            <>
              /// rightsizing · {pct} + {headroom}% headroom · {window} observed · memory sizes to max(p99, observed max)
            </>
          }
          title="Requests that match what containers actually use."
          sub="Every recommendation is a proposal. Open a row for the reasoning and a guarded RightsizingPolicy — the operator applies it only after a human arms it."
          actions={<DataSourceBadge source={res.source} reason={res.reason} detail={res.detail} />}
        />

        <StatGrid cols={5}>
          <StatTile label="Savings / mo" value={formatUsd(savings)} sub={`${downs.length} downsizes`} tone="var(--color-signal)" icon={ArrowDownRight} />
          <StatTile label="CPU reclaimable" value={`${formatCores(cores)} cores`} sub="requests above p95 + headroom" icon={Gauge} />
          <StatTile label="Memory reclaimable" value={formatBytes(bytes)} sub="never below observed max" icon={MemoryStick} />
          <StatTile
            label="Upsizes"
            value={String(ups.length)}
            sub="under-provisioned · reliability first"
            tone={ups.length > 0 ? "var(--color-warn)" : "var(--color-fg)"}
            icon={ArrowUpRight}
          />
          <StatTile
            label="OOM-killed"
            value={String(ooms.length)}
            sub={ooms.length > 0 ? `${ooms.reduce((s, r) => s + r.oomKills, 0)} kills in window` : "no OOM kills"}
            tone={ooms.length > 0 ? "var(--color-accent)" : "var(--color-fg)"}
            icon={AlertOctagon}
          />
        </StatGrid>

        <div className="mb-3 mt-6 flex flex-wrap items-center gap-2">
          <Segmented param="window" label="observation window" value={window} defaultValue="7d" options={["24h", "7d", "30d"].map((w) => ({ value: w, label: w }))} />
          <Segmented
            param="pct"
            label="cpu percentile"
            value={pct}
            defaultValue="p95"
            options={["p90", "p95", "p99", "max"].map((p) => ({ value: p, label: p, title: `size CPU to ${p}` }))}
          />
          <ParamSelect
            param="headroom"
            label="headroom"
            value={String(headroom)}
            defaultValue="15"
            options={[0, 10, 15, 25, 50].map((h) => ({ value: String(h), label: `${h}%` }))}
          />
          <ParamSelect param="direction" label="direction" value={dirFilter} allLabel="all" options={[{ value: "downsize", label: "downsize" }, { value: "upsize", label: "upsize" }]} />
          <ParamSelect param="conf" label="confidence" value={confFilter} allLabel="any" options={[{ value: "medium", label: "medium +" }, { value: "high", label: "high" }]} />
          <ParamSelect param="cluster" label="cluster" value={cluster} allLabel="all" clear={["namespace"]} options={clusters.map((c) => ({ value: c, label: c }))} />
          <ParamSelect param="namespace" label="namespace" value={namespace} allLabel="all" options={namespaces.map((n) => ({ value: n, label: n }))} />
        </div>
        <TableFilter placeholder="search workload, namespace, cluster or container… ( / )" />

        <Panel
          title={`/// recommendations · ${rows.length} of ${all.length}`}
          meta={<UsageDistLegend />}
          footer={
            <>
              <span>kubehero rightsize list --window {window} --percentile {pct}</span>
              <span>apply = RightsizingPolicy · human-armed · reversible</span>
            </>
          }
        >
          {rows.length === 0 ? (
            all.length === 0 ? (
              <EmptyState
                icon={Scale}
                title="No recommendations — requests already fit"
                body="Either every container is within 10% of its measured p95 (+ headroom), or there isn't enough usage history yet. Recommendations need at least a day of container usage from the collector."
                cta={{ href: "/allocation", label: "see allocation" }}
              />
            ) : (
              <EmptyState icon={Scale} title="Nothing matches these filters" cta={{ href: PATH, label: "reset filters" }} compact />
            )
          ) : (
            <div className="max-h-[72vh] overflow-auto">
              <table className="w-full min-w-[1060px] border-collapse text-[12.5px]" data-nav-list>
                <thead>
                  <tr className="border-b border-[var(--color-line)]">
                    <SortHeader column="workload" label="workload · container" active={sort.key === "workload"} dir={sort.dir} className="pl-4" />
                    <SortHeader column="cpu" label="cpu request → rec" active={sort.key === "cpu"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="mem" label="memory request → rec" active={sort.key === "mem"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="savings" label="savings/mo" align="right" active={sort.key === "savings"} dir={sort.dir} className="pr-3" />
                    <SortHeader column="confidence" label="confidence" active={sort.key === "confidence"} dir={sort.dir} className="pr-3" />
                    <Th className="pr-4">signal</Th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((r) => (
                    <RecRow key={r.id} r={r} href={hrefWith(PATH, params, { rec: r.id })} active={r.id === recId} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </Panel>
      </div>

      {selected && (
        <UrlDrawer
          param="rec"
          width={620}
          title={`${selected.namespace}/${selected.workload}`}
          subtitle={`${selected.cluster} · container ${selected.container} · ${selected.replicas}× ${selected.workloadKind}`}
        >
          <RecDetail r={selected} siblings={siblings} headroom={headroom} />
        </UrlDrawer>
      )}
    </>
  );
}

async function siblingWorkloads(r: Rightsizing, forceDemo: boolean): Promise<string[]> {
  const res = await getAllocation({
    window: "7d",
    aggregate: "workload",
    filters: { cluster: r.cluster, namespace: r.namespace },
    forceDemo,
  });
  return res.data.rows.filter((x) => !x.isIdle).map((x) => x.properties.workload || x.name);
}

function dirTone(d: Rightsizing["direction"]): string {
  return d === "downsize" ? "var(--color-signal)" : d === "upsize" ? "var(--color-warn)" : "var(--color-fg-faint)";
}
const CONF_TONE = { low: "var(--color-fg-faint)", medium: "var(--color-cool)", high: "var(--color-signal)" };

function RecRow({ r, href, active }: { r: Rightsizing; href: string; active: boolean }) {
  return (
    <tr
      className={`group border-b border-[var(--color-line)] transition-colors last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50 focus-within:bg-[var(--color-bg-sunken)]/50 ${
        active ? "bg-[var(--color-bg-sunken)]/70" : ""
      }`}
    >
      <td className="max-w-[300px] py-2.5 pl-4 pr-3">
        <Link href={href} scroll={false} data-nav-item className="block min-w-0 focus-visible:outline-none">
          <span className="block truncate font-mono text-[12.5px] text-[var(--color-fg)] group-hover:text-[var(--color-cool)]">
            {r.namespace}/{r.workload}
          </span>
          <span className="block truncate font-mono text-[10.5px] text-[var(--color-fg-faint)]">
            {r.cluster} · {r.container} · {r.replicas}×
          </span>
        </Link>
      </td>
      <td className="py-2.5 pr-3">
        <div className="flex items-center gap-3">
          <span className="w-[92px] font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">
            {formatCores(r.cpu.request)} → <span style={{ color: dirTone(r.cpu.recommended < r.cpu.request ? "downsize" : r.cpu.recommended > r.cpu.request ? "upsize" : "ok") }}>{formatCores(r.cpu.recommended)}</span>
          </span>
          <UsageDist label="cpu" format={formatCores} p50={r.cpu.p50} p95={r.cpu.p95} p99={r.cpu.p99} max={r.cpu.max} request={r.cpu.request} recommended={r.cpu.recommended} limit={r.cpu.limit} />
        </div>
      </td>
      <td className="py-2.5 pr-3">
        <div className="flex items-center gap-3">
          <span className="w-[128px] font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">
            {formatBytes(r.mem.request)} → <span style={{ color: dirTone(r.mem.recommended < r.mem.request ? "downsize" : r.mem.recommended > r.mem.request ? "upsize" : "ok") }}>{formatBytes(r.mem.recommended)}</span>
          </span>
          <UsageDist label="memory" format={(v) => formatBytes(v)} p50={r.mem.p50} p95={r.mem.p99} p99={r.mem.p99} max={r.mem.max} request={r.mem.request} recommended={r.mem.recommended} limit={r.mem.limit} />
        </div>
      </td>
      <td className="py-2.5 pr-3 text-right font-mono tabular-nums" style={{ color: r.savingsUsdMonth >= 0 ? "var(--color-signal)" : "var(--color-warn)" }}>
        {r.savingsUsdMonth >= 0 ? formatUsd(r.savingsUsdMonth) : `+${formatUsd(-r.savingsUsdMonth)}`}
      </td>
      <td className="py-2.5 pr-3">
        <Chip tone={CONF_TONE[r.confidence]}>{r.confidence}</Chip>
      </td>
      <td className="py-2.5 pr-4">
        <span className="flex flex-wrap gap-1.5">
          <Chip tone={dirTone(r.direction)}>{r.direction}</Chip>
          {r.oomKills > 0 && (
            <Chip tone="var(--color-accent)" title={`${r.oomKills} OOM kills in the window`}>
              oom × {r.oomKills}
            </Chip>
          )}
          {r.cpuThrottleRisk >= 0.1 && (
            <Chip tone="var(--color-warn)" title="share of 5-minute buckets where p99 usage hit ≥ 90% of the CPU limit">
              throttle {formatPct(r.cpuThrottleRisk)}
            </Chip>
          )}
        </span>
      </td>
    </tr>
  );
}

function RecDetail({ r, siblings, headroom }: { r: Rightsizing; siblings: string[]; headroom: number }) {
  const hub = workloadHref(r.cluster, r.namespace, r.workload);
  return (
    <div className="flex flex-col gap-5 px-5 py-4">
      <div className="grid grid-cols-3 gap-px border border-[var(--color-line)] bg-[var(--color-line)]">
        <Cell label="savings / mo" value={r.savingsUsdMonth >= 0 ? formatUsd(r.savingsUsdMonth) : `+${formatUsd(-r.savingsUsdMonth)} cost`} tone={r.savingsUsdMonth >= 0 ? "var(--color-signal)" : "var(--color-warn)"} />
        <Cell label="current → proposed" value={`${formatUsd(r.currentCostUsdMonth)} → ${formatUsd(r.recommendedCostUsdMonth)}`} />
        <Cell label="confidence" value={`${r.confidence} · ${r.samples.toLocaleString("en-US")} samples`} tone={CONF_TONE[r.confidence]} />
      </div>

      <section>
        <h3 className="section-label mb-2">/// why</h3>
        <p className="text-[13px] leading-relaxed text-[var(--color-fg-dim)]">{r.reason || "No explanation returned."}</p>
      </section>

      <section>
        <h3 className="section-label mb-2">/// measured usage · {r.window}</h3>
        <table className="w-full border-collapse font-mono text-[11.5px] tabular-nums">
          <thead>
            <tr className="text-left text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">
              <th className="py-1 font-normal" />
              {["p50", "p95", "p99", "max", "request", "limit", "proposed"].map((h) => (
                <th key={h} className="py-1 text-right font-normal">
                  {h}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="text-[var(--color-fg-dim)]">
            <tr className="border-t border-[var(--color-line)]">
              <td className="py-1.5 text-[var(--color-fg-faint)]">cpu</td>
              {[r.cpu.p50, r.cpu.p95, r.cpu.p99, r.cpu.max, r.cpu.request, r.cpu.limit].map((v, i) => (
                <td key={i} className="py-1.5 text-right">{v > 0 ? formatCores(v) : "—"}</td>
              ))}
              <td className="py-1.5 text-right text-[var(--color-signal)]">{formatCores(r.cpu.recommended)}</td>
            </tr>
            <tr className="border-t border-[var(--color-line)]">
              <td className="py-1.5 text-[var(--color-fg-faint)]">mem</td>
              {[r.mem.p50, 0, r.mem.p99, r.mem.max, r.mem.request, r.mem.limit].map((v, i) => (
                <td key={i} className="py-1.5 text-right">{v > 0 ? formatBytes(v) : "—"}</td>
              ))}
              <td className="py-1.5 text-right text-[var(--color-signal)]">{formatBytes(r.mem.recommended)}</td>
            </tr>
          </tbody>
        </table>
        <div className="mt-3 flex flex-col gap-2">
          <div className="flex items-center gap-3 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">
            <span className="w-8">cpu</span>
            <UsageDist width={420} label="cpu" format={formatCores} p50={r.cpu.p50} p95={r.cpu.p95} p99={r.cpu.p99} max={r.cpu.max} request={r.cpu.request} recommended={r.cpu.recommended} limit={r.cpu.limit} />
          </div>
          <div className="flex items-center gap-3 font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">
            <span className="w-8">mem</span>
            <UsageDist width={420} label="memory" format={(v) => formatBytes(v)} p50={r.mem.p50} p95={r.mem.p99} p99={r.mem.p99} max={r.mem.max} request={r.mem.request} recommended={r.mem.recommended} limit={r.mem.limit} />
          </div>
          <UsageDistLegend />
        </div>
      </section>

      <section>
        <h3 className="section-label mb-2">/// apply via policy</h3>
        <PolicyPanel
          input={{
            cluster: r.cluster,
            namespace: r.namespace,
            workload: r.workload,
            container: r.container,
            window: r.window,
            headroomPct: headroom,
            replicas: r.replicas,
            siblings,
            cpuRecommended: formatCores(r.cpu.recommended),
            memRecommended: formatBytes(r.mem.recommended),
            savingsUsdMonth: r.savingsUsdMonth,
          }}
        />
      </section>

      <section className="flex flex-wrap gap-2">
        <DrawerLink href={hub}>workload hub</DrawerLink>
        <DrawerLink href={logsHref(logqlSelector({ cluster: r.cluster, namespace: r.namespace, workload: r.workload }), { window: "24h" })}>logs</DrawerLink>
        <DrawerLink href={`/profiles?service=${encodeURIComponent(r.workload)}&namespace=${encodeURIComponent(r.namespace)}`}>cpu profile</DrawerLink>
        <DrawerLink href={`/ask?q=${encodeURIComponent(`Is it safe to rightsize ${r.namespace}/${r.workload}?`)}&context=${encodeURIComponent(hub)}`}>ask kubehero</DrawerLink>
      </section>
    </div>
  );
}

function Cell({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div className="flex flex-col gap-1 bg-[var(--color-bg-raised)] px-3 py-2.5">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">{label}</span>
      <span className="font-mono text-[13px] tabular-nums" style={{ color: tone ?? "var(--color-fg)" }}>
        {value}
      </span>
    </div>
  );
}

function DrawerLink({ href, children }: { href: string; children: React.ReactNode }) {
  return (
    <Link
      href={href}
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
    >
      {children}
      <ArrowUpRight className="h-3 w-3" aria-hidden />
    </Link>
  );
}
