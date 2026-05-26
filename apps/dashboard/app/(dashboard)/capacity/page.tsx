// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ArrowRight, Clock, Cpu, Layers } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { getCapacity, type CapacityDemand } from "@/lib/api/capacity";

export const metadata = { title: "Capacity · KubeHero" };
export const dynamic = "force-dynamic";

// Ray's autoscaler dashboard borrowed pattern: surface the workload's
// own perspective ("I'm asking for X but the cluster doesn't have X")
// rather than the scheduler's perspective ("here are unschedulable pods,
// good luck"). Then bind the recommendation to dollars on both sides
// — what's blocked AND what unblocking costs — so the operator can
// make the trade in one screen.

export default async function CapacityPage() {
  const { demands, totalPendingPods, totalBlockedUsdMonth, source } = await getCapacity({ limit: 50 });
  const totalRecommendedK = demands.reduce((s, d) => s + d.recommendedCostUsdMonth, 0) / 1000;
  const ratio = totalRecommendedK > 0 ? totalBlockedUsdMonth / 1000 / totalRecommendedK : 0;

  return (
    <>
      <Topbar crumbs={[{ label: "capacity" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Clock className="h-3 w-3 text-[var(--color-warn)]" />
              /// pending demand · {totalPendingPods} pods waiting · ${(totalBlockedUsdMonth / 1000).toFixed(1)}k/mo blocked
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Workloads waiting on capacity.
            </h1>
            <p className="mt-2 max-w-2xl text-[14px] text-[var(--color-fg-dim)]">
              Each row pairs the cost of doing nothing with the cost of unblocking it.
              When the multiplier on the right is &gt;1×, scaling out probably pays for itself
              within the month.
            </p>
          </div>
          <DataSourceBadge source={source} />
        </div>

        {/* headline ratio banner */}
        <div className="mb-6 grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3">
          <Banner
            label="Blocked work / mo"
            value={`$${(totalBlockedUsdMonth / 1000).toFixed(1)}k`}
            tone="var(--color-accent)"
            icon={Cpu}
          />
          <Banner
            label="Capacity ask / mo"
            value={`$${totalRecommendedK.toFixed(1)}k`}
            tone="var(--color-cool)"
            icon={Layers}
          />
          <Banner
            label="Multiplier"
            value={`${ratio.toFixed(1)}×`}
            tone={ratio >= 1 ? "var(--color-signal)" : "var(--color-fg-dim)"}
            icon={ArrowRight}
            sub={ratio >= 1 ? "scaling pays for itself" : "review the table — adding capacity may not be the right move"}
          />
        </div>

        {/* demand rows */}
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// blocked workloads · ranked by $ blocked
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              source · kube-scheduler events · 30s window
            </span>
          </div>
          {demands.length === 0 ? (
            <div className="px-4 py-12 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
              No pending demand. The scheduler is keeping up.{" "}
              <Link href="/fleet" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                see the fleet →
              </Link>
            </div>
          ) : (
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {[...demands]
                .sort((a, b) => b.blockedCostUsdMonth - a.blockedCostUsdMonth)
                .map((d, i) => (
                  <DemandRow key={d.id} rank={i + 1} demand={d} />
                ))}
            </div>
          )}
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>kubehero capacity scale --apply</span>
            <Link href="/docs/integrations/clouds" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              autoscaler integration docs →
            </Link>
          </div>
        </div>
      </div>
    </>
  );
}

function Banner({
  label,
  value,
  sub,
  tone,
  icon: Icon,
}: {
  label: string;
  value: string;
  sub?: string;
  tone: string;
  icon: React.ElementType;
}) {
  return (
    <div className="flex flex-col gap-1.5 bg-[var(--color-bg-raised)] px-5 py-4">
      <div className="flex items-center justify-between">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          {label}
        </span>
        <Icon className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" />
      </div>
      <span
        className="font-mono text-[24px] tabular-nums tracking-tight"
        style={{ color: tone }}
      >
        {value}
      </span>
      {sub && (
        <span className="font-mono text-[10.5px] text-[var(--color-fg-dim)]">
          {sub}
        </span>
      )}
    </div>
  );
}

function DemandRow({ rank, demand: d }: { rank: number; demand: CapacityDemand }) {
  const ratio = d.recommendedCostUsdMonth > 0 ? d.blockedCostUsdMonth / d.recommendedCostUsdMonth : 0;
  const ratioTone = ratio >= 2 ? "var(--color-signal)" : ratio >= 1 ? "var(--color-cool)" : "var(--color-fg-dim)";
  return (
    <div className="grid gap-3 bg-[var(--color-bg-raised)] px-4 py-3 sm:grid-cols-[2.5rem_minmax(0,1fr)_minmax(0,1.2fr)_minmax(0,1.4fr)_minmax(0,1.4fr)] sm:items-start">
      <span className="font-mono text-[10px] tabular-nums text-[var(--color-fg-faint)]">
        {String(rank).padStart(2, "0")}
      </span>

      <div className="flex flex-col gap-1 min-w-0">
        <Link
          href={`/clusters/${encodeURIComponent(d.cluster)}`}
          className="truncate font-mono text-[12.5px] text-[var(--color-fg)] hover:text-[var(--color-cool)]"
        >
          {d.workload}
        </Link>
        <span className="truncate font-mono text-[10.5px] text-[var(--color-fg-dim)]">
          {d.cluster} · {d.namespace}
        </span>
      </div>

      <div className="flex flex-col gap-1 font-mono text-[11px]">
        <span className="text-[var(--color-fg-dim)]">
          <span className="text-[var(--color-fg-faint)]">cpu</span> {d.requestedCpu}
        </span>
        <span className="text-[var(--color-fg-dim)]">
          <span className="text-[var(--color-fg-faint)]">mem</span> {d.requestedMem}
        </span>
        {d.requestedGpu && (
          <span className="text-[var(--color-warn)]">
            <span className="text-[var(--color-fg-faint)]">gpu</span> {d.requestedGpu}
          </span>
        )}
      </div>

      <div className="flex flex-col gap-1 font-mono text-[11px]">
        <span className="text-[var(--color-accent)]">
          <span className="text-[var(--color-fg-faint)]">blocked</span> ${(d.blockedCostUsdMonth / 1000).toFixed(1)}k / mo
        </span>
        <span className="text-[var(--color-fg-dim)]">
          {d.pendingPods} pod{d.pendingPods === 1 ? "" : "s"} pending · oldest {d.oldestPendingAge}
        </span>
      </div>

      <div className="flex flex-col gap-1 font-mono text-[11px]">
        <span className="text-[var(--color-cool)]">
          <span className="text-[var(--color-fg-faint)]">unblock for</span> ${(d.recommendedCostUsdMonth / 1000).toFixed(1)}k / mo
        </span>
        <span className="truncate text-[var(--color-fg-dim)]" title={d.recommendedAction}>
          {d.recommendedAction}
        </span>
        <span className="font-medium" style={{ color: ratioTone }}>
          {ratio >= 1 ? `pays back ${ratio.toFixed(1)}× in month 1` : "review before scaling"}
        </span>
      </div>
    </div>
  );
}
