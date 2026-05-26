// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { notFound } from "next/navigation";
import { ArrowLeft, ChevronRight, Flame, History } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { cloudColor } from "@/lib/fleet-data";
import { getWorkloadDetail } from "@/lib/api/waste";
import type { AuditEntry } from "@/lib/api/audit";

export const dynamic = "force-dynamic";

type Params = Promise<{ slug: string[] }>;

function outcomeColor(o: AuditEntry["outcome"]) {
  switch (o) {
    case "applied":  return "var(--color-signal)";
    case "armed":    return "var(--color-cool)";
    case "cooldown": return "var(--color-warn)";
    case "reverted": return "var(--color-fg-dim)";
  }
}

export default async function WorkloadPage({ params }: { params: Params }) {
  const { slug } = await params;
  if (!Array.isArray(slug) || slug.length < 3) notFound();

  const [cluster, namespace, ...nameParts] = slug.map(decodeURIComponent);
  const name = nameParts.join("/");
  const detail = await getWorkloadDetail({ cluster, namespace, name });
  if (!detail) notFound();

  const { rec, history, source } = detail;

  return (
    <>
      <Topbar
        crumbs={[
          { label: "waste", href: "/waste" },
          { label: cluster },
          { label: `${namespace}/${name}` },
        ]}
      />
      <div className="px-5 py-6">
        <Link
          href="/waste"
          className="mb-4 inline-flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] transition-colors hover:text-[var(--color-fg)]"
        >
          <ArrowLeft className="h-3 w-3" /> back to waste
        </Link>

        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-3">
              <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
                cluster · {cluster}
                <ChevronRight className="mx-1 inline h-3 w-3 text-[var(--color-fg-faint)]" />
                ns · {namespace}
              </span>
              {rec && (
                <span
                  className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
                  style={{ color: cloudColor[rec.cloud] }}
                >
                  <span className="h-1.5 w-1.5" style={{ background: cloudColor[rec.cloud] }} aria-hidden />
                  {rec.cloud}
                </span>
              )}
            </div>
            <h1 className="font-mono text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              {name}
            </h1>
          </div>
          <DataSourceBadge source={source} />
        </div>

        {/* current rec */}
        {rec ? (
          <div className="mb-6 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
              <span className="inline-flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                <Flame className="h-3 w-3 text-[var(--color-accent)]" />
                /// current recommendation
              </span>
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                action · <span style={{ color: rec.tone }}>{rec.action}</span>
              </span>
            </div>
            <div className="grid gap-[1px] bg-[var(--color-line)] sm:grid-cols-3">
              <Cell label="signal" value={rec.detail} />
              <Cell label="recoverable" value={`$${rec.amountK.toFixed(1)}k / mo`} tone={rec.tone} />
              <Cell label="rank" value={rec.rank} mono />
            </div>
          </div>
        ) : (
          <div className="mb-6 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-4 py-6 font-mono text-[12px] text-[var(--color-fg-dim)]">
            No active recommendation for this workload. The history below is
            still available for audit purposes.
          </div>
        )}

        {/* history */}
        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
            <span className="inline-flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <History className="h-3 w-3" />
              /// history · {history.length} events
            </span>
          </div>
          {history.length === 0 ? (
            <div className="px-4 py-6 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
              no events yet
            </div>
          ) : (
            <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
              {history.map((e) => (
                <div
                  key={e.id}
                  className="flex flex-wrap items-center gap-3 bg-[var(--color-bg-raised)] px-4 py-3 font-mono text-[11px]"
                >
                  <span className="w-40 shrink-0 tabular-nums text-[var(--color-fg-faint)]">
                    {e.at}
                  </span>
                  <span
                    className="w-20 shrink-0 uppercase tracking-[0.14em]"
                    style={{ color: outcomeColor(e.outcome) }}
                  >
                    {e.outcome}
                  </span>
                  <span className="min-w-[200px] flex-1 text-[var(--color-fg-dim)]">
                    {e.action}
                  </span>
                  {e.effectK !== undefined && (
                    <span className="w-24 shrink-0 text-right tabular-nums text-[var(--color-signal)]">
                      −${e.effectK.toFixed(1)}k/mo
                    </span>
                  )}
                  <span className="w-28 shrink-0 text-right text-[var(--color-fg-faint)]">
                    {e.id}
                  </span>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>
    </>
  );
}

function Cell({
  label,
  value,
  tone,
  mono,
}: {
  label: string;
  value: string;
  tone?: string;
  mono?: boolean;
}) {
  return (
    <div className="flex flex-col gap-1 bg-[var(--color-bg-raised)] px-4 py-3">
      <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        {label}
      </span>
      <span
        className={`text-[14px] ${mono ? "font-mono tabular-nums" : ""}`}
        style={tone ? { color: tone } : undefined}
      >
        {value}
      </span>
    </div>
  );
}
