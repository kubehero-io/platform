// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /logs — LogQL explorer. Everything is in the URL (query, range, tab,
// limit), so any view is a link: overview cards, workload hubs, alerts
// and the /ask agent all deep-link here with a query pre-filled.

import Link from "next/link";
import { Filter, ScrollText } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { PageHeader } from "@/components/ui/page-header";
import { Panel } from "@/components/ui/panel";
import { UrlTabs } from "@/components/ui/url-tabs";
import { ParamSelect, ParamToggle, Segmented } from "@/components/ui/param-controls";
import { Chip } from "@/components/ui/chip";
import { EmptyState } from "@/components/ui/empty-state";
import { Sparkline } from "@/components/sparkline";
import { QueryEditor } from "@/components/logs/query-editor";
import { LogList } from "@/components/logs/log-list";
import { LiveTail } from "@/components/logs/live-tail";
import { VolumeChart } from "@/components/logs/volume-chart";
import { MetricChart } from "@/components/logs/metric-chart";
import { getLogPatterns, getLogVolume, listLogLabels, queryLogs } from "@/lib/api/logs";
import { combineSources } from "@/lib/api/source";
import { LEVEL_COLOR, normalizeLevel } from "@/lib/chart/palette";
import { formatBytes, formatCompact, formatPct, formatUsd } from "@/lib/chart/scale";
import { innerLogQuery, isMetricQuery, patternToRegex } from "@/lib/logql/tokenize";
import { autoStep, resolveRange } from "@/lib/time-range";
import { flatParams, hrefWith, param, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Logs · KubeHero" };
export const dynamic = "force-dynamic";

const PATH = "/logs";
const WINDOWS = ["15m", "1h", "6h", "24h", "7d"] as const;
const DEFAULT_QUERY = '{level=~"error|fatal"}';

export default async function LogsPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const params = flatParams(sp);
  const query = param(sp, "q", 4096) || DEFAULT_QUERY;
  const metric = isMetricQuery(query);
  const logQuery = innerLogQuery(query);
  const range = resolveRange(sp, { defaultWindow: "1h", allowed: WINDOWS });
  const span = range.endMs - range.startMs;
  const volumeStep = autoStep(span, 60);
  const metricStep = autoStep(span, 120);
  const tabParam = param(sp, "tab");
  const tab = tabParam === "patterns" || tabParam === "metrics" || tabParam === "lines" ? tabParam : metric ? "metrics" : "lines";
  const limit = [100, 500, 1000, 5000].includes(Number(param(sp, "limit"))) ? Number(param(sp, "limit")) : 500;
  const direction = param(sp, "dir") === "forward" ? "forward" : "backward";
  const liveTail = param(sp, "live") === "1" && !metric;
  const forceDemo = param(sp, "demo") === "1";

  const [volume, labels, lines, matrix, patterns] = await Promise.all([
    getLogVolume({ query: logQuery, range, stepMs: volumeStep, forceDemo }),
    listLogLabels({ range, forceDemo }),
    tab === "lines" && !metric && !liveTail ? queryLogs({ query, range, limit, direction, stepMs: metricStep, forceDemo }) : null,
    tab === "metrics" && metric ? queryLogs({ query, range, limit, direction, stepMs: metricStep, forceDemo }) : null,
    tab === "patterns" ? getLogPatterns({ query: logQuery, range, limit: 50, forceDemo }) : null,
  ]);

  const src = combineSources(volume, ...[lines, matrix, patterns].filter((x): x is NonNullable<typeof x> => x !== null));
  const queryError = lines?.queryError ?? matrix?.queryError ?? patterns?.queryError ?? volume.queryError;
  const v = volume.data;
  const errors = v.series.filter((s) => ["error", "fatal"].includes(normalizeLevel(s.key))).reduce((a, s) => a + s.values.reduce((x, y) => x + y, 0), 0);
  const stats = lines?.data.kind === "streams" ? lines.data.stats : matrix?.data.kind === "matrix" ? matrix.data.stats : null;

  const tabs = [
    { id: "lines", label: "lines", count: lines?.data.kind === "streams" ? lines.data.lines.length : undefined },
    { id: "patterns", label: "patterns", count: patterns ? patterns.data.patterns.length : undefined },
    { id: "metrics", label: "metrics" },
  ];

  return (
    <>
      <Topbar crumbs={[{ label: "observe" }, { label: "logs" }]} range={{ options: WINDOWS, defaultWindow: "1h", custom: true, refresh: true }} />
      <div className="px-5 py-6">
        <PageHeader
          icon={ScrollText}
          eyebrow={<>/// logs · loki-compatible logql · {range.label}</>}
          title="Every log line, one query away."
          actions={<DataSourceBadge {...src} />}
        />

        <QueryEditor key={query} initial={query} labelNames={labels.data.names} error={queryError} />

        {/* volume */}
        <Panel
          className="mt-4"
          title="/// volume by level"
          meta={
            <span className="inline-flex flex-wrap gap-x-3">
              <span>{formatCompact(v.totalLines)} lines</span>
              <span>{formatBytes(v.totalBytes)}</span>
              {v.totalLines > 0 && <span style={{ color: errors / v.totalLines > 0.05 ? "var(--color-warn)" : undefined }}>{formatPct(errors / v.totalLines, 1)} errors</span>}
              {v.estCostUsdMonth > 0 && <span title="What storing this volume costs per month at the configured $/GB">≈ {formatUsd(v.estCostUsdMonth, { cents: true })}/mo to store</span>}
            </span>
          }
          bodyClassName="px-4 pb-2 pt-3"
        >
          <VolumeChart times={v.times} stepMs={v.stepMs} series={v.series} startMs={range.startMs} endMs={range.endMs} />
          <div className="mt-1 font-mono text-[10px] text-[var(--color-fg-faint)]">drag across the histogram to zoom · {range.custom ? <Link className="text-[var(--color-cool)] hover:text-[var(--color-fg)]" href={hrefWith(PATH, params, { from: null, to: null })}>reset zoom</Link> : "step " + Math.round(v.stepMs / 1000) + "s"}</div>
        </Panel>

        {/* results */}
        <div className="mt-4 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex flex-wrap items-center justify-between gap-2 pr-3">
            <UrlTabs pathname={PATH} params={params} tabs={tabs} active={tab} label="result views" />
            <div className="flex flex-wrap items-center gap-2 py-1.5">
              {tab === "lines" && !metric && (
                <>
                  <ParamToggle param="live" label="live tail" checked={liveTail} title="Stream new lines as they arrive" />
                  {!liveTail && (
                    <>
                      <ParamSelect param="limit" label="limit" value={String(limit)} defaultValue="500" options={["100", "500", "1000", "5000"].map((x) => ({ value: x, label: x }))} />
                      <Segmented param="dir" label="order" value={direction} defaultValue="backward" options={[{ value: "backward", label: "newest" }, { value: "forward", label: "oldest" }]} />
                    </>
                  )}
                </>
              )}
              {stats && (
                <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  {formatCompact(stats.rowsScanned)} rows · {formatBytes(stats.bytesScanned)} · {stats.execMs.toFixed(0)}ms
                </span>
              )}
            </div>
          </div>

          {tab === "lines" &&
            (metric ? (
              <EmptyState compact icon={Filter} title="This is a metric query" body="Metric queries return series, not lines — see the metrics tab." cta={{ href: hrefWith(PATH, params, { tab: "metrics" }), label: "open metrics" }} />
            ) : liveTail ? (
              <LiveTail query={query} />
            ) : (
              <LogList
                lines={lines?.data.kind === "streams" ? lines.data.lines : []}
                query={query}
                emptyText={queryError ? "Fix the query above to see lines." : "No lines match in this range — widen the window or loosen a filter."}
              />
            ))}

          {tab === "patterns" && patterns && <Patterns data={patterns.data} logQuery={logQuery} params={params} />}

          {tab === "metrics" &&
            (metric && matrix?.data.kind === "matrix" ? (
              <div className="px-4 pb-3 pt-4">
                <MetricChart
                  times={matrix.data.metric.times}
                  series={foldSeries(matrix.data.metric.series)}
                  startMs={range.startMs}
                  endMs={range.endMs}
                  unit={/\brate\(|bytes_rate\(/.test(query) ? "/s" : ""}
                />
                <MetricTable series={matrix.data.metric.series} />
              </div>
            ) : (
              <MetricHelpers logQuery={logQuery} params={params} />
            ))}
        </div>
      </div>
    </>
  );
}

/** ≤ 8 series on one chart: the 7 largest + "other". */
function foldSeries<T extends { key: string; label: string; labels: Record<string, string>; values: (number | null)[] }>(series: T[]): T[] {
  if (series.length <= 8) return series;
  const total = (s: T) => s.values.reduce<number>((a, b) => a + (b ?? 0), 0);
  const sorted = [...series].sort((a, b) => total(b) - total(a));
  const keep = sorted.slice(0, 7);
  const rest = sorted.slice(7);
  const other = { ...rest[0], key: "other", label: `other (${rest.length})`, labels: {}, values: rest[0].values.map((_, i) => rest.reduce((a, s) => a + (s.values[i] ?? 0), 0)) };
  return [...keep, other];
}

function MetricTable({ series }: { series: { key: string; label: string; values: (number | null)[] }[] }) {
  const last = (vs: (number | null)[]) => [...vs].reverse().find((v) => v !== null) ?? null;
  const rows = [...series].sort((a, b) => (last(b.values) ?? 0) - (last(a.values) ?? 0)).slice(0, 50);
  if (rows.length === 0) return null;
  return (
    <table className="mt-4 w-full border-collapse font-mono text-[12px]">
      <thead>
        <tr className="border-b border-[var(--color-line)] text-left text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          <th className="py-1.5 font-normal">series</th>
          <th className="py-1.5 text-right font-normal">last</th>
          <th className="py-1.5 text-right font-normal">max</th>
        </tr>
      </thead>
      <tbody>
        {rows.map((s) => (
          <tr key={s.key} className="border-b border-[var(--color-line)] last:border-b-0">
            <td className="max-w-[520px] truncate py-1.5 text-[var(--color-fg-dim)]">{s.label}</td>
            <td className="py-1.5 text-right tabular-nums text-[var(--color-fg)]">{last(s.values) === null ? "—" : formatCompact(last(s.values)!, 2)}</td>
            <td className="py-1.5 text-right tabular-nums text-[var(--color-fg-dim)]">{formatCompact(Math.max(0, ...s.values.map((v) => v ?? 0)), 2)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function MetricHelpers({ logQuery, params }: { logQuery: string; params: Record<string, string> }) {
  const helpers = [
    { label: "lines per minute by level", q: `sum by (level) (count_over_time(${logQuery} [1m]))` },
    { label: "error rate by workload", q: `sum by (workload) (rate(${logQuery} |~ "(?i)error|fatal" [5m]))` },
    { label: "top 10 pods by volume", q: `topk(10, sum by (pod) (count_over_time(${logQuery} [5m])))` },
    { label: "bytes per second by namespace", q: `sum by (namespace) (bytes_rate(${logQuery} [5m]))` },
  ];
  return (
    <div className="px-4 py-5">
      <p className="mb-3 max-w-2xl text-[13px] text-[var(--color-fg-dim)]">
        The current query returns lines. Wrap it in a range function to chart it — pick one to start:
      </p>
      <div className="grid gap-2 md:grid-cols-2">
        {helpers.map((h) => (
          <Link
            key={h.label}
            href={hrefWith(PATH, params, { q: h.q, tab: "metrics" })}
            className="group border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-2 transition-colors hover:border-[var(--color-cool)]"
          >
            <div className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] group-hover:text-[var(--color-cool)]">{h.label}</div>
            <div className="mt-1 truncate font-mono text-[11.5px] text-[var(--color-fg-dim)]">{h.q}</div>
          </Link>
        ))}
      </div>
    </div>
  );
}

function Patterns({
  data,
  logQuery,
  params,
}: {
  data: { patterns: { pattern: string; count: number; level: string; sharePct: number; trend: number[]; sample: string }[]; linesAnalyzed: number };
  logQuery: string;
  params: Record<string, string>;
}) {
  if (data.patterns.length === 0) {
    return <EmptyState compact icon={Filter} title="No patterns in this range" body="Patterns cluster matching lines into templates — widen the range or the selector." />;
  }
  const max = Math.max(...data.patterns.map((p) => p.sharePct), 1);
  return (
    <div className="max-h-[640px] overflow-auto">
      <table className="w-full min-w-[900px] border-collapse text-[12px]">
        <thead>
          <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pl-4 font-normal">pattern · {formatCompact(data.linesAnalyzed)} lines analyzed</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 font-normal">level</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 text-right font-normal">count</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 font-normal">share</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-3 font-normal">trend</th>
            <th className="sticky top-0 bg-[var(--color-bg-raised)] py-2 pr-4 font-normal" />
          </tr>
        </thead>
        <tbody>
          {data.patterns.map((p) => {
            const level = normalizeLevel(p.level);
            const re = patternToRegex(p.pattern);
            const lit = re.includes("`") ? `"${re.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"` : `\`${re}\``;
            return (
              <tr key={p.pattern} className="group border-b border-[var(--color-line)] align-top last:border-b-0 hover:bg-[var(--color-bg-sunken)]/50">
                <td className="max-w-[560px] py-2 pl-4 pr-3">
                  <div className="break-all font-mono text-[12px] leading-[18px] text-[var(--color-fg)]">
                    {p.pattern.split("<_>").map((part, i, arr) => (
                      <span key={i}>
                        {part}
                        {i < arr.length - 1 && <span className="rounded-[2px] bg-[var(--color-line-bright)] px-0.5 text-[var(--color-fg-faint)]">&lt;_&gt;</span>}
                      </span>
                    ))}
                  </div>
                  <div className="mt-1 hidden truncate font-mono text-[10.5px] text-[var(--color-fg-faint)] group-hover:block" title={p.sample}>
                    e.g. {p.sample}
                  </div>
                </td>
                <td className="py-2 pr-3">
                  <Chip tone={LEVEL_COLOR[level]}>{level}</Chip>
                </td>
                <td className="py-2 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">{formatCompact(p.count)}</td>
                <td className="py-2 pr-3">
                  <span className="inline-flex items-center gap-2">
                    <span className="relative inline-block h-[5px] w-20 bg-[var(--color-line)]" aria-hidden>
                      <span className="absolute inset-y-0 left-0" style={{ width: `${(p.sharePct / max) * 100}%`, background: LEVEL_COLOR[level] }} />
                    </span>
                    <span className="w-12 font-mono text-[11px] tabular-nums text-[var(--color-fg-dim)]">{p.sharePct.toFixed(p.sharePct < 1 ? 2 : 1)}%</span>
                  </span>
                </td>
                <td className="py-2 pr-3">
                  <Sparkline values={p.trend} width={96} height={22} color={LEVEL_COLOR[level]} ariaLabel={`${p.pattern} trend`} />
                </td>
                <td className="py-2 pr-4 text-right">
                  <Link
                    href={hrefWith(PATH, params, { q: `${logQuery} |~ ${lit}`, tab: "lines" })}
                    className="inline-flex items-center gap-1 whitespace-nowrap border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
                  >
                    <Filter className="h-3 w-3" aria-hidden /> filter
                  </Link>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
