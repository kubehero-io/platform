// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { notFound } from "next/navigation";
import { ArrowRight } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { CLUSTERS, cloudColor, stateMeta } from "@/lib/fleet-data";
import { getCluster } from "@/lib/api/clusters";

// Pre-render the well-known demo set; live clusters lazily render on first request.
export function generateStaticParams() {
  return CLUSTERS.map((c) => ({ id: c.id }));
}

export const dynamicParams = true;
export const dynamic = "force-dynamic";

type Params = { id: string };

const waste = [
  { rank: "01", name: "model-server-a100",  ns: "ml-inference", detail: "gpu=8  util=12%",         amount: "$18,200 / mo", tone: "var(--color-accent)", action: "apply"  },
  { rank: "02", name: "vectordb-ingress",   ns: "retrieval",    detail: "cpu.req=16  used=0.41",   amount: "$8,640 / mo",  tone: "var(--color-accent)", action: "apply"  },
  { rank: "03", name: "jobs-etl-nightly",   ns: "data",         detail: "limit=32cpu  burst=2.1",  amount: "overcommit: high", tone: "var(--color-warn)", action: "review" },
  { rank: "04", name: "frontend-gateway",   ns: "edge",         detail: "cpu.req=2  used=1.6",     amount: "right-sized",  tone: "var(--color-signal)", action: null    },
];

export default async function ClusterPage({
  params,
}: {
  params: Promise<Params>;
}) {
  const { id } = await params;
  const { cluster: c, source } = await getCluster(id);
  if (!c) notFound();
  const state = stateMeta[c.state];

  return (
    <>
      <Topbar
        crumbs={[
          { label: "fleet", href: "/fleet" },
          { label: c.name },
        ]}
      />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-3">
              <span
                className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
                style={{ color: cloudColor[c.cloud] }}
              >
                <span className="h-1.5 w-1.5" style={{ background: cloudColor[c.cloud] }} aria-hidden />
                {c.cloud}
              </span>
              <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
                {c.region} · {c.nodes} nodes · {c.gpu}
              </span>
            </div>
            <h1 className="font-mono text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              {c.name}
            </h1>
          </div>
          <div className="flex items-center gap-3">
            <span
              className="inline-flex items-center gap-1.5 font-mono text-[10.5px] uppercase tracking-[0.12em]"
              style={{ color: state.color }}
            >
              <span className="h-1.5 w-1.5" style={{ background: state.color }} aria-hidden />
              {state.label}
            </span>
            <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
              {c.recoverable} recoverable · {c.costDay}/day
            </span>
            <DataSourceBadge source={source} />
          </div>
        </div>

        {/* waste list */}
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// top waste · 7d window
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              last scan · 12s ago
            </span>
          </div>
          <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
            {waste.map((w) => (
              <div
                key={w.rank}
                className="flex items-start gap-3 bg-[var(--color-bg-raised)] px-4 py-3"
              >
                <span className="mt-0.5 font-mono text-[10px] tabular-nums text-[var(--color-fg-faint)]">
                  {w.rank}
                </span>
                <div className="min-w-0 flex-1">
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="truncate font-mono text-[12.5px] text-[var(--color-fg)]">
                      {w.name}
                    </span>
                    <span
                      className="shrink-0 font-mono text-[11px] tabular-nums"
                      style={{ color: w.tone }}
                    >
                      {w.amount}
                    </span>
                  </div>
                  <div className="mt-0.5 flex items-baseline justify-between gap-2 font-mono text-[10.5px] text-[var(--color-fg-dim)]">
                    <span>
                      <span className="text-[var(--color-fg-faint)]">ns:</span>{" "}
                      {w.ns}
                    </span>
                    <span>{w.detail}</span>
                  </div>
                </div>
                {w.action && (
                  <button
                    type="button"
                    className="shrink-0 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
                    style={{
                      color: w.action === "apply" ? "var(--color-cool)" : "var(--color-warn)",
                    }}
                  >
                    → {w.action}
                  </button>
                )}
              </div>
            ))}
          </div>
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>total recoverable · $28,960 / mo</span>
            <span className="inline-flex items-center gap-1.5 text-[var(--color-cool)]">
              kubehero rightsize --apply
              <ArrowRight className="h-3 w-3" />
            </span>
          </div>
        </div>
      </div>
    </>
  );
}
