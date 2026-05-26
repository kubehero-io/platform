// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ExternalLink, ShieldAlert } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { Sparkline, syntheticSeries } from "@/components/sparkline";
import { TableFilter } from "@/components/table-filter";
import { getPosture, type Vulnerability } from "@/lib/api/posture";

export const metadata = { title: "Posture · KubeHero" };
export const dynamic = "force-dynamic";

type SearchParams = Promise<{ q?: string; severity?: string; source?: string; window?: string }>;

const severityColor = (s: string) => {
  switch (s) {
    case "critical": return "var(--color-accent)";
    case "high":     return "var(--color-warn)";
    case "medium":   return "var(--color-cool)";
    case "low":      return "var(--color-fg-dim)";
    default:         return "var(--color-fg-faint)";
  }
};

export default async function PosturePage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const win = sp.window === "24h" || sp.window === "7d" ? sp.window : "30d";
  const { vulnerabilities: all, counts, source } = await getPosture({
    severity: sp.severity,
    limit: 100,
  });

  const q = (sp.q ?? "").toLowerCase().trim();
  const sourceFilter = sp.source ?? "";
  const vulns = all.filter((v) => {
    if (q && ![v.workload, v.namespace, v.cluster, v.image, v.packageName, v.id]
      .some((s) => s.toLowerCase().includes(q))) return false;
    if (sourceFilter && v.source !== sourceFilter) return false;
    return true;
  });

  // Cost × CVE ranking: workloads where the per-month spend AND the
  // CVSS are both high. Used for the headline KPI.
  const ranked = [...vulns].sort(
    (a, b) => b.cvss * b.costUsdMonth - a.cvss * a.costUsdMonth,
  );
  const totalAtRisk = ranked.reduce((s, v) => s + v.costUsdMonth, 0);

  return (
    <>
      <Topbar crumbs={[{ label: "posture" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <ShieldAlert className="h-3 w-3 text-[var(--color-warn)]" />
              /// posture · cost × CVE · {win} · {ranked.length} of {all.length} findings
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Vulnerable AND expensive workloads.
            </h1>
            <p className="mt-2 max-w-2xl text-[14px] text-[var(--color-fg-dim)]">
              Sources: Trivy Operator (CVEs), Defender / Inspector / SCC
              (cloud posture), Falco (runtime). Findings are joined with
              per-workload monthly spend so you can fix the
              <span className="text-[var(--color-fg)]"> $18k/mo critical first</span>,
              not the $200/mo critical first.
            </p>
          </div>
          <DataSourceBadge source={source} />
        </div>

        {/* severity KPIs */}
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          <SeverityKpi label="critical" tone="var(--color-accent)" count={counts.critical} />
          <SeverityKpi label="high"     tone="var(--color-warn)"   count={counts.high} />
          <SeverityKpi label="medium"   tone="var(--color-cool)"   count={counts.medium} />
          <SeverityKpi label="low"      tone="var(--color-fg-dim)" count={counts.low} />
        </div>

        <div className="mt-6">
          <TableFilter
            placeholder="search workload, namespace, cluster, image, package, CVE…"
            facets={[
              {
                key: "severity",
                label: "severity",
                options: [
                  { value: "critical", label: "critical" },
                  { value: "high",     label: "high" },
                  { value: "medium",   label: "medium" },
                  { value: "low",      label: "low" },
                ],
              },
              {
                key: "source",
                label: "source",
                options: [
                  { value: "trivy",     label: "trivy" },
                  { value: "defender",  label: "defender" },
                  { value: "inspector", label: "inspector" },
                  { value: "gcp-scc",   label: "gcp-scc" },
                ],
              },
            ]}
          />
        </div>

        {/* table */}
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// findings · ranked by cost × CVSS
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              total at-risk · ${(totalAtRisk / 1000).toFixed(1)}k / mo
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[920px] border-collapse text-[13px]">
              <thead>
                <tr className="border-b border-[var(--color-line)] text-left font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                  <th className="py-2 pl-4 font-normal">CVE</th>
                  <th className="py-2 font-normal">Sev</th>
                  <th className="py-2 font-normal">Workload</th>
                  <th className="py-2 font-normal">Image / package</th>
                  <th className="py-2 pr-3 text-right font-normal">CVSS</th>
                  <th className="py-2 pr-3 text-right font-normal">$ / mo</th>
                  <th className="py-2 pr-4 text-right font-normal">Fix</th>
                </tr>
              </thead>
              <tbody>
                {ranked.length === 0 && (
                  <tr>
                    <td colSpan={7} className="px-4 py-8 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
                      no findings match this filter ·{" "}
                      <Link href="/posture" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                        clear filter →
                      </Link>
                    </td>
                  </tr>
                )}
                {ranked.map((v) => (
                  <Row key={`${v.id}-${v.workload}`} v={v} />
                ))}
              </tbody>
            </table>
          </div>
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>kubehero scan --posture · audit-logged</span>
            <Link
              href="/docs/posture"
              className="inline-flex items-center gap-1.5 text-[var(--color-cool)] hover:text-[var(--color-fg)]"
            >
              how cost × CVE works <ExternalLink className="h-3 w-3" />
            </Link>
          </div>
        </div>
      </div>
    </>
  );
}

function SeverityKpi({
  label,
  count,
  tone,
}: {
  label: string;
  count: number;
  tone: string;
}) {
  const trend = syntheticSeries({ seed: `cve-${label}`, center: Math.max(count, 1), variance: 0.18, trend: label === "critical" ? 0.005 : 0 });
  return (
    <div className="flex flex-col gap-1.5 bg-[var(--color-bg-raised)] px-5 py-4">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        {label}
      </span>
      <div className="flex items-baseline justify-between gap-2">
        <span
          className="font-mono text-[24px] tabular-nums tracking-tight"
          style={{ color: tone }}
        >
          {count}
        </span>
        <Sparkline values={trend} color={tone} width={72} height={22} ariaLabel={`${label} trend`} />
      </div>
    </div>
  );
}

function Row({ v }: { v: Vulnerability }) {
  const href = `/workloads/${encodeURIComponent(v.cluster)}/${encodeURIComponent(v.namespace)}/${encodeURIComponent(v.workload)}`;
  return (
    <tr className="border-b border-[var(--color-line)] last:border-b-0 hover:bg-[var(--color-bg-sunken)]/40">
      <td className="py-3 pl-4 font-mono text-[12px] text-[var(--color-fg-dim)]">
        {v.id}
      </td>
      <td className="py-3 pr-3">
        <span
          className="inline-flex items-center gap-1.5 font-mono text-[10.5px] uppercase tracking-[0.12em]"
          style={{ color: severityColor(v.severity) }}
        >
          <span className="h-1.5 w-1.5" style={{ background: severityColor(v.severity) }} aria-hidden />
          {v.severity}
        </span>
      </td>
      <td className="py-3 pr-3">
        <Link href={href} className="font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]">
          {v.workload}
          <span className="ml-1 text-[var(--color-fg-faint)]">·</span>
          <span className="ml-1 text-[11px] text-[var(--color-fg-dim)]">{v.namespace}</span>
        </Link>
      </td>
      <td className="py-3 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">
        <div className="truncate" title={v.image}>
          {v.image}
        </div>
        <div className="text-[10.5px] text-[var(--color-fg-faint)]">
          {v.packageName} · {v.installedVersion}
          {v.fixedVersion ? ` → ${v.fixedVersion}` : ""}
        </div>
      </td>
      <td className="py-3 pr-3 text-right font-mono tabular-nums" style={{ color: severityColor(v.severity) }}>
        {v.cvss.toFixed(1)}
      </td>
      <td className="py-3 pr-3 text-right font-mono tabular-nums text-[var(--color-fg)]">
        ${(v.costUsdMonth / 1000).toFixed(1)}k
      </td>
      <td className="py-3 pr-4 text-right font-mono text-[11px]">
        {v.fixedVersion
          ? <span className="text-[var(--color-signal)]">available</span>
          : <span className="text-[var(--color-fg-faint)]">—</span>}
      </td>
    </tr>
  );
}
