// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import {
  ArrowRight,
  ArrowUpRight,
  Bell,
  Cpu,
  DollarSign,
  Flame,
  ShieldAlert,
} from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { Sparkline, syntheticSeries } from "@/components/sparkline";
import { getFleet } from "@/lib/api/clusters";
import { getWaste } from "@/lib/api/waste";
import { getPolicies } from "@/lib/api/policies";
import { getPosture } from "@/lib/api/posture";
import { getAnomalies, type Anomaly } from "@/lib/api/anomalies";
import type { Vulnerability } from "@/lib/api/posture";

export const metadata = { title: "Overview · KubeHero" };
export const dynamic = "force-dynamic";

type SearchParams = Promise<{ window?: string }>;

export default async function OverviewPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const win = sp.window === "24h" || sp.window === "7d" ? sp.window : "30d";

  // Fetch every signal in parallel — server components run them
  // concurrently, and we degrade-gracefully if any individual one is
  // empty (which is the typical experience until ingest lands).
  const [fleet, waste, policies, posture, anomalies] = await Promise.all([
    getFleet(),
    getWaste({ limit: 10 }),
    getPolicies(),
    getPosture({ limit: 50 }),
    getAnomalies({ window: win, limit: 4 }),
  ]);

  // Headline KPIs
  const recoverableK = waste.rows.reduce((s, r) => s + r.amountK, 0);
  const fleetCostDayK = fleet.clusters.reduce(
    (s, c) => s + parseDollarK(c.costDay),
    0,
  );
  const fleetCostMonthK = fleetCostDayK * 30;
  const gpuIdleK = waste.rows
    .filter((r) => r.detail.toLowerCase().includes("gpu") || r.detail.toLowerCase().includes("util"))
    .reduce((s, r) => s + r.amountK, 0);
  const armedCount = policies.rows.filter((p) => p.spentPct > 0 && p.kind).length;

  // Action queue ranks the highest-impact next steps the operator has
  // available right now. Three sources, one ranking.
  const actions = [
    ...waste.rows.slice(0, 3).map((r) => ({
      id: `waste-${r.workload}`,
      kind: "waste" as const,
      title: `Apply rightsize · ${r.workload}`,
      subtitle: `${r.cluster} · ${r.ns}`,
      impactK: r.amountK,
      verb: "review",
      href: `/workloads/${encodeURIComponent(r.cluster)}/${encodeURIComponent(r.ns)}/${encodeURIComponent(r.workload)}`,
    })),
    ...policies.rows
      .filter((p) => p.spentPct >= 70)
      .slice(0, 2)
      .map((p) => ({
        id: `policy-${p.name}`,
        kind: "policy" as const,
        title: `Arm ceiling · ${p.name}`,
        subtitle: p.scope,
        impactK: p.ceilingUSD * (1 - p.spentPct / 100) / 1000,
        verb: "arm",
        href: `/budgets`,
      })),
    ...posture.vulnerabilities.slice(0, 2).map((v: Vulnerability) => ({
      id: `cve-${v.id}`,
      kind: "posture" as const,
      title: `Patch ${v.id} · ${v.workload}`,
      subtitle: `${v.image} · CVSS ${v.cvss.toFixed(1)}`,
      impactK: v.costUsdMonth / 1000,
      verb: "review",
      href: `/posture?q=${encodeURIComponent(v.id)}`,
    })),
  ]
    .sort((a, b) => b.impactK - a.impactK)
    .slice(0, 5);

  // Synthetic 30d series — deterministic per KPI so screenshots stay
  // consistent. Replace with ListSpendSeries when ingest lands.
  const trendSpend = syntheticSeries({ seed: "spend-mtd", center: fleetCostMonthK, variance: 0.06, trend: 0.003 });
  const trendRecoverable = syntheticSeries({ seed: "recoverable-week", center: recoverableK, variance: 0.18, trend: -0.002 });
  const trendGpu = syntheticSeries({ seed: "gpu-idle", center: gpuIdleK, variance: 0.10, trend: 0.001 });
  const trendArmed = syntheticSeries({ seed: "armed-policies", center: Math.max(armedCount, 1), variance: 0.04 });

  return (
    <>
      <Topbar crumbs={[{ label: "overview" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// command center · {win}
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              What changed. What it costs. What to do next.
            </h1>
          </div>
          <DataSourceBadge source={anomalies.source} />
        </div>

        {/* row 1 — KPIs with sparklines */}
        <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-2 lg:grid-cols-4">
          <Kpi
            label="Monthly spend"
            value={`$${fleetCostMonthK.toFixed(0)}k`}
            sub={`${win} avg · ${fleet.clusters.length} clusters`}
            tone="var(--color-fg)"
            icon={DollarSign}
            series={trendSpend}
          />
          <Kpi
            label="Recoverable now"
            value={`$${recoverableK.toFixed(1)}k`}
            sub={`${waste.rows.length} workloads ranked`}
            tone="var(--color-accent)"
            icon={Flame}
            series={trendRecoverable}
            seriesColor="var(--color-accent)"
          />
          <Kpi
            label="GPU idle"
            value={`$${gpuIdleK.toFixed(1)}k`}
            sub="last 30 days"
            tone="var(--color-warn)"
            icon={Cpu}
            series={trendGpu}
            seriesColor="var(--color-warn)"
          />
          <Kpi
            label="Policies armed"
            value={`${armedCount}`}
            sub={`${policies.rows.length} total`}
            tone="var(--color-signal)"
            icon={ShieldAlert}
            series={trendArmed}
            seriesColor="var(--color-signal)"
          />
        </div>

        {/* row 2 — action queue */}
        <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// action queue · ranked by $ / mo
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              top 5 of {waste.rows.length + policies.rows.length + posture.vulnerabilities.length}
            </span>
          </div>
          {actions.length === 0 ? (
            <EmptyAction />
          ) : (
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {actions.map((a, i) => (
                <ActionRow key={a.id} rank={i + 1} action={a} />
              ))}
            </div>
          )}
        </div>

        {/* row 3 — anomalies */}
        <div className="mt-6 grid gap-4 lg:grid-cols-3">
          {anomalies.anomalies.slice(0, 3).map((a) => (
            <AnomalyCard key={a.id} anomaly={a} />
          ))}
          {anomalies.anomalies.length === 0 && <EmptyAnomalies />}
        </div>

        {/* row 4 — actionable forecast */}
        <Forecast
          monthlySpendK={fleetCostMonthK}
          recoverableK={recoverableK}
          unarmedHighRiskK={
            policies.rows
              .filter((p) => p.spentPct >= 70 && p.kind === "CeilingPolicy")
              .reduce((s, p) => s + p.ceilingUSD * (1 - p.spentPct / 100), 0) / 1000
          }
        />
      </div>
    </>
  );
}

function Kpi({
  label,
  value,
  sub,
  tone,
  icon: Icon,
  series,
  seriesColor,
}: {
  label: string;
  value: string;
  sub: string;
  tone: string;
  icon: React.ElementType;
  series: number[];
  seriesColor?: string;
}) {
  return (
    <div className="flex flex-col gap-2 bg-[var(--color-bg-raised)] px-5 py-4">
      <div className="flex items-center justify-between">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          {label}
        </span>
        <Icon className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" />
      </div>
      <div className="flex items-baseline justify-between gap-2">
        <span
          className="font-mono text-[24px] tabular-nums tracking-tight"
          style={{ color: tone }}
        >
          {value}
        </span>
        <Sparkline values={series} color={seriesColor ?? tone} width={88} height={26} ariaLabel={`${label} 30d trend`} />
      </div>
      <div className="font-mono text-[11px] text-[var(--color-fg-dim)]">
        {sub}
      </div>
    </div>
  );
}

type Action = {
  id: string;
  kind: "waste" | "policy" | "posture";
  title: string;
  subtitle: string;
  impactK: number;
  verb: string;
  href: string;
};

function ActionRow({ rank, action }: { rank: number; action: Action }) {
  const tone =
    action.kind === "waste" ? "var(--color-accent)"
    : action.kind === "policy" ? "var(--color-cool)"
    : "var(--color-warn)";
  return (
    <Link
      href={action.href}
      className="group flex items-center gap-4 bg-[var(--color-bg-raised)] px-4 py-3 transition-colors hover:bg-[var(--color-bg-sunken)]/60"
    >
      <span className="w-8 shrink-0 font-mono text-[10px] tabular-nums text-[var(--color-fg-faint)]">
        {String(rank).padStart(2, "0")}
      </span>
      <span
        className="inline-flex w-16 shrink-0 items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.12em]"
        style={{ color: tone }}
      >
        <span className="h-1.5 w-1.5" style={{ background: tone }} aria-hidden />
        {action.kind}
      </span>
      <div className="min-w-0 flex-1">
        <div className="truncate font-mono text-[12.5px] text-[var(--color-fg)]">
          {action.title}
        </div>
        <div className="truncate font-mono text-[10.5px] text-[var(--color-fg-dim)]">
          {action.subtitle}
        </div>
      </div>
      <span className="shrink-0 font-mono text-[12px] tabular-nums" style={{ color: tone }}>
        ${action.impactK.toFixed(1)}k / mo
      </span>
      <span className="shrink-0 inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors group-hover:border-[var(--color-cool)] group-hover:text-[var(--color-cool)]">
        {action.verb}
        <ArrowRight className="h-3 w-3" />
      </span>
    </Link>
  );
}

function AnomalyCard({ anomaly }: { anomaly: Anomaly }) {
  const tone = anomaly.severity === "critical" ? "var(--color-accent)"
    : anomaly.severity === "warn" ? "var(--color-warn)"
    : "var(--color-cool)";
  return (
    <Link
      href={anomaly.linkPath}
      className="flex flex-col gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-4 transition-colors hover:border-[var(--color-cool)]"
    >
      <div className="flex items-center justify-between">
        <span
          className="inline-flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.14em]"
          style={{ color: tone }}
        >
          <Bell className="h-3 w-3" />
          {anomaly.kind}
        </span>
        {anomaly.deltaPct !== 0 && (
          <span className="font-mono text-[11px] tabular-nums" style={{ color: tone }}>
            {anomaly.deltaPct > 0 ? "+" : ""}
            {anomaly.deltaPct.toFixed(0)}%
          </span>
        )}
      </div>
      <div className="font-mono text-[13.5px] leading-snug text-[var(--color-fg)]">
        {anomaly.title}
      </div>
      <div className="font-mono text-[11px] leading-snug text-[var(--color-fg-dim)]">
        {anomaly.detail}
      </div>
      <div className="flex items-center justify-between font-mono text-[11px]">
        <span className="text-[var(--color-fg-faint)]">{anomaly.subject}</span>
        <span className="inline-flex items-center gap-1 text-[var(--color-cool)]">
          investigate <ArrowUpRight className="h-3 w-3" />
        </span>
      </div>
    </Link>
  );
}

function Forecast({
  monthlySpendK,
  recoverableK,
  unarmedHighRiskK,
}: {
  monthlySpendK: number;
  recoverableK: number;
  unarmedHighRiskK: number;
}) {
  const projected = monthlySpendK + monthlySpendK * 0.04; // current trajectory
  const ifAllArmed = projected - unarmedHighRiskK;
  const ifAllRightsized = ifAllArmed - recoverableK;

  const Row = ({
    label,
    valueK,
    delta,
    href,
    cta,
    tone,
  }: {
    label: string;
    valueK: number;
    delta?: string;
    href: string;
    cta: string;
    tone: string;
  }) => (
    <div className="flex flex-wrap items-baseline gap-3 border-b border-[var(--color-line)] px-4 py-3 last:border-b-0">
      <span className="min-w-[260px] font-mono text-[12px] text-[var(--color-fg-dim)]">
        {label}
      </span>
      <span className="font-mono text-[18px] tabular-nums" style={{ color: tone }}>
        ${valueK.toFixed(0)}k / mo
      </span>
      {delta && (
        <span className="font-mono text-[11px] text-[var(--color-fg-faint)]">
          {delta}
        </span>
      )}
      <Link
        href={href}
        className="ml-auto inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
      >
        {cta}
        <ArrowRight className="h-3 w-3" />
      </Link>
    </div>
  );

  return (
    <div className="mt-8 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
      <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          /// next-quarter forecast · with verbs
        </span>
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          assumes constant fleet
        </span>
      </div>
      <Row
        label="At current trajectory you'll hit"
        valueK={projected}
        delta="+4% vs trailing month"
        href="/chargeback"
        cta="see breakdown"
        tone="var(--color-warn)"
      />
      <Row
        label="If you arm every high-risk ceiling, projected"
        valueK={ifAllArmed}
        delta={`saves ~$${unarmedHighRiskK.toFixed(0)}k/mo`}
        href="/budgets"
        cta="review policies"
        tone="var(--color-cool)"
      />
      <Row
        label="With every flagged rightsize applied"
        valueK={ifAllRightsized}
        delta={`saves another ~$${recoverableK.toFixed(0)}k/mo`}
        href="/waste"
        cta="review waste"
        tone="var(--color-signal)"
      />
    </div>
  );
}

function EmptyAction() {
  return (
    <div className="bg-[var(--color-bg-raised)] px-4 py-6 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
      Nothing actionable right now.
      {" "}
      <Link href="/waste" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
        run a scan →
      </Link>
    </div>
  );
}

function EmptyAnomalies() {
  return (
    <div className="col-span-3 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-4 py-6 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
      No anomalies detected this window.
    </div>
  );
}

// parseDollarK turns "$8,640" or "$8.64k" into thousands. The fleet
// demo data uses the raw-string form; once ingest lands these become
// real numeric series.
function parseDollarK(s: string): number {
  const cleaned = s.replace(/[$,]/g, "").trim().toLowerCase();
  if (cleaned.endsWith("k")) return Number(cleaned.slice(0, -1));
  if (cleaned.endsWith("m")) return Number(cleaned.slice(0, -1)) * 1000;
  return Number(cleaned) / 1000;
}
