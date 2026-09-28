// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /profiles — continuous profiling, priced. Pick a service (with its CPU
// $/mo), a profile type, optionally a baseline window for a diff, and
// read the flamegraph + top functions in $/month.

import Link from "next/link";
import { Flame, GitCompare } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { ParamSelect, ParamToggle, Segmented } from "@/components/ui/param-controls";
import { SortHeader, Th } from "@/components/ui/sort-header";
import { Chip } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { Flamegraph } from "@/components/flamegraph/flamegraph";
import { getFlamegraph, getTopFunctions, listProfileTargets } from "@/lib/api/profiles";
import { combineSources } from "@/lib/api/source";
import { formatAgo, formatCores, formatUsd } from "@/lib/chart/scale";
import { formatProfileValue } from "@/lib/profiles/map";
import { parseSort, sortRows } from "@/lib/table-sort";
import { DAY, resolveRange } from "@/lib/time-range";
import { flatParams, hrefWith, param, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Profiles · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/profiles";
const WINDOWS = ["15m", "1h", "6h", "24h", "7d"] as const;
const ORIGIN_TONE: Record<string, string> = {
  ebpf: "var(--color-cool)",
  "pprof-scrape": "var(--color-signal)",
  "pprof-push": "var(--color-signal)",
  pyroscope: "var(--color-warn)",
};
const TYPE_LABEL: Record<string, string> = {
  cpu: "cpu",
  alloc_space: "alloc bytes",
  alloc_objects: "alloc objects",
  inuse_space: "in-use bytes",
  inuse_objects: "in-use objects",
  goroutines: "goroutines",
  mutex: "mutex",
  block: "block",
};

export default async function ProfilesPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const range = resolveRange(sp, { defaultWindow: "1h", allowed: WINDOWS });
  const forceDemo = param(sp, "demo") === "1";
  const nsFilter = param(sp, "namespace", 63);

  const targets = await listProfileTargets({ range, forceDemo });
  const all = targets.data;
  const service = param(sp, "service", 253);
  const selected =
    all.find((t) => t.service === service && (!nsFilter || t.namespace === nsFilter)) ?? (service ? undefined : all[0]);
  const types = selected?.types.length ? selected.types : ["cpu"];
  const type = types.includes(param(sp, "type")) ? param(sp, "type") : types.includes("cpu") ? "cpu" : types[0];
  const diff = param(sp, "diff") === "1";
  const baselineKind = ["previous", "1d", "7d"].includes(param(sp, "baseline")) ? param(sp, "baseline") : "previous";
  const span = range.endMs - range.startMs;
  const shift = baselineKind === "1d" ? DAY : baselineKind === "7d" ? 7 * DAY : span;
  const baseline = diff ? { startMs: range.startMs - shift, endMs: range.endMs - shift } : undefined;
  const fn = param(sp, "fn", 400);

  const [flame, top] = selected
    ? await Promise.all([
        getFlamegraph({ service: selected.service, namespace: selected.namespace, type, range, baseline, forceDemo: forceDemo || targets.source === "demo" }),
        getTopFunctions({ service: selected.service, namespace: selected.namespace, type, range, limit: 50, forceDemo: forceDemo || targets.source === "demo" }),
      ])
    : [null, null];
  const src = combineSources(targets, ...(flame ? [flame] : []), ...(top ? [top] : []));
  const fg = flame?.data ?? null;

  const sort = parseSort(sp, ["self", "total", "cost", "name"] as const, { key: "self", dir: "desc" });
  const fns = top ? sortRows(top.data, sort, { self: (f) => f.self, total: (f) => f.total, cost: (f) => f.selfCostUsdMonth, name: (f) => f.name }) : [];

  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "profiles" }]} range={{ options: WINDOWS, defaultWindow: "1h", custom: true, refresh: true }} />
      <div className="px-5 py-6">
        <PageHeader
          icon={Flame}
          iconTone="var(--color-warn)"
          eyebrow={<>/// profiles · ebpf + pprof + pyroscope · {range.label}</>}
          title="Where the CPU — and the money — goes."
          sub="Every frame is priced: a function's share of the workload's CPU time × what that CPU costs per month."
          actions={<DataSourceBadge {...src} />}
        />

        {all.length === 0 ? (
          <Panel title="/// no profiles yet">
            <EmptyState
              icon={Flame}
              title="No profiling data in this window"
              body="KubeHero's collector profiles every pod with eBPF when enabled, and scrapes pprof endpoints from annotated pods. Pyroscope SDKs can push to /ingest."
              code={`# scrape a Go service's pprof endpoint\nkubectl annotate pod <pod> profiles.grafana.com/cpu.scrape=true profiles.grafana.com/cpu.port=6060\n\n# or profile everything with eBPF\nhelm upgrade kubehero kubehero/kubehero --set collector.profiling.ebpf=true`}
              cta={{ href: hrefWith(PATH, params, { demo: "1" }), label: "preview with demo data" }}
            />
          </Panel>
        ) : (
          <div className="grid gap-4 xl:grid-cols-[300px_minmax(0,1fr)]">
            {/* service picker */}
            <Panel title={`/// services · ${all.length}`} meta="cpu $/mo" bodyClassName="max-h-[76vh] overflow-y-auto">
              <ul className="flex flex-col">
                {all.map((t) => {
                  const active = selected?.service === t.service && selected?.namespace === t.namespace;
                  return (
                    <li key={`${t.namespace}/${t.service}`}>
                      <Link
                        href={hrefWith(PATH, params, { service: t.service, namespace: t.namespace, type: null, fn: null })}
                        aria-current={active ? "true" : undefined}
                        data-nav-item
                        className={`flex flex-col gap-1 border-b border-l-2 border-b-[var(--color-line)] px-3 py-2 transition-colors hover:bg-[var(--color-bg-sunken)]/60 focus-visible:bg-[var(--color-bg-sunken)] focus-visible:outline-none ${
                          active ? "border-l-[var(--color-fg)] bg-[var(--color-bg-sunken)]/70" : "border-l-transparent"
                        }`}
                      >
                        <span className="flex items-baseline justify-between gap-2">
                          <span className="truncate font-mono text-[12.5px] text-[var(--color-fg)]">{t.service}</span>
                          <span className="shrink-0 font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">{t.costUsdMonth > 0 ? formatUsd(t.costUsdMonth) : "—"}</span>
                        </span>
                        <span className="flex items-center justify-between gap-2 font-mono text-[10.5px] text-[var(--color-fg-faint)]">
                          <span className="truncate">
                            {t.namespace} · {t.cpuCoresAvg > 0 ? `${formatCores(t.cpuCoresAvg)} cores` : "—"}
                          </span>
                          <Chip tone={ORIGIN_TONE[t.origin] ?? "var(--color-fg-faint)"} className="!px-1 !py-0 !text-[9px]">
                            {t.origin}
                          </Chip>
                        </span>
                      </Link>
                    </li>
                  );
                })}
              </ul>
            </Panel>

            <div className="flex min-w-0 flex-col gap-4">
              {selected && (
                <div className="flex flex-wrap items-center gap-2">
                  <span className="mr-1 font-mono text-[13px] text-[var(--color-fg)]">
                    {selected.namespace}/{selected.service}
                  </span>
                  <Segmented param="type" label="profile type" value={type} defaultValue={types.includes("cpu") ? "cpu" : types[0]} options={types.map((t) => ({ value: t, label: TYPE_LABEL[t] ?? t }))} clear={["fn"]} />
                  <ParamToggle param="diff" label="diff" checked={diff} title="Compare against a baseline window" />
                  {diff && (
                    <ParamSelect
                      param="baseline"
                      label="baseline"
                      value={baselineKind}
                      defaultValue="previous"
                      options={[
                        { value: "previous", label: "previous period" },
                        { value: "1d", label: "same time yesterday" },
                        { value: "7d", label: "same time last week" },
                      ]}
                    />
                  )}
                  {selected.lastSeenMs > 0 && (
                    <span className="ml-auto font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                      last sample {formatAgo(Date.now() - selected.lastSeenMs)} ago
                    </span>
                  )}
                </div>
              )}

              <Panel
                title={`/// flamegraph · ${TYPE_LABEL[type] ?? type}${diff ? " · diff" : ""}`}
                meta={
                  fg ? (
                    <>
                      {formatProfileValue(fg.total, fg.unit)} total{fg.costUsdMonth > 0 ? ` · ${formatUsd(fg.costUsdMonth)}/mo cpu` : ""}
                      {diff && fg.baselineTotal === 0 ? " · no baseline data" : ""}
                    </>
                  ) : undefined
                }
                actions={diff ? <GitCompare className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" aria-hidden /> : undefined}
                bodyClassName="p-4"
              >
                {fg ? (
                  <Flamegraph key={`${selected?.service}|${type}|${fn}`} nodes={fg.nodes} unit={fg.unit} costUsdMonth={fg.costUsdMonth} baselineTotal={diff ? fg.baselineTotal : 0} highlight={fn} />
                ) : (
                  <EmptyState compact icon={Flame} title="No samples for this service and type in the window" body="Widen the time range, or pick another profile type." />
                )}
              </Panel>

              <Panel
                title="/// top functions"
                meta={type === "cpu" ? "self $/mo = self share × workload cpu spend" : undefined}
                footer={<span>kubehero profile top --service {selected?.service} --type {type}</span>}
              >
                {fns.length === 0 ? (
                  <EmptyState compact title="No functions" />
                ) : (
                  <div className="max-h-[520px] overflow-auto">
                    <table className="w-full min-w-[720px] border-collapse text-[12px]">
                      <thead>
                        <tr className="border-b border-[var(--color-line)]">
                          <SortHeader column="name" label="function" active={sort.key === "name"} dir={sort.dir} className="pl-4" />
                          <SortHeader column="self" label="self" align="right" active={sort.key === "self"} dir={sort.dir} className="pr-3" />
                          <Th align="right" className="pr-3">self %</Th>
                          <SortHeader column="total" label="total" align="right" active={sort.key === "total"} dir={sort.dir} className="pr-3" />
                          <Th align="right" className="pr-3">total %</Th>
                          {type === "cpu" && <SortHeader column="cost" label="self $/mo" align="right" active={sort.key === "cost"} dir={sort.dir} className="pr-4" />}
                        </tr>
                      </thead>
                      <tbody>
                        {fns.map((f) => (
                          <tr key={f.name} className={`border-b border-[var(--color-line)] last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50 ${fn === f.name ? "bg-[var(--color-bg-sunken)]/70" : ""}`}>
                            <td className="max-w-[460px] py-1.5 pl-4 pr-3">
                              <Link
                                href={hrefWith(PATH, params, { fn: fn === f.name ? null : f.name })}
                                scroll={false}
                                data-nav-item
                                title="highlight in the flamegraph"
                                className="block truncate font-mono text-[12px] text-[var(--color-fg)] hover:text-[var(--color-cool)]"
                              >
                                {f.name}
                              </Link>
                            </td>
                            <td className="py-1.5 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">{fg ? formatProfileValue(f.self, fg.unit) : f.self}</td>
                            <td className="py-1.5 pr-3 text-right">
                              <span className="inline-flex items-center gap-2">
                                <span className="relative inline-block h-[4px] w-14 bg-[var(--color-line)]" aria-hidden>
                                  <span className="absolute inset-y-0 left-0 bg-[var(--color-warn)]" style={{ width: `${Math.min(100, f.selfPct)}%` }} />
                                </span>
                                <span className="w-12 font-mono text-[11px] tabular-nums text-[var(--color-fg-dim)]">{f.selfPct.toFixed(1)}%</span>
                              </span>
                            </td>
                            <td className="py-1.5 pr-3 text-right font-mono tabular-nums text-[var(--color-fg-dim)]">{fg ? formatProfileValue(f.total, fg.unit) : f.total}</td>
                            <td className="py-1.5 pr-3 text-right font-mono text-[11px] tabular-nums text-[var(--color-fg-dim)]">{f.totalPct.toFixed(1)}%</td>
                            {type === "cpu" && (
                              <td className="py-1.5 pr-4 text-right font-mono tabular-nums text-[var(--color-signal)]">
                                {f.selfCostUsdMonth > 0 ? formatUsd(f.selfCostUsdMonth, { cents: true }) : "—"}
                              </td>
                            )}
                          </tr>
                        ))}
                      </tbody>
                    </table>
                  </div>
                )}
              </Panel>
            </div>
          </div>
        )}
      </div>
    </>
  );
}
