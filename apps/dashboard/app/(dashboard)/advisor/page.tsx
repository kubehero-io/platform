// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Sparkles } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { ActionCard } from "@/components/advisor/action-card";
import { BriefingMarkdown } from "@/components/advisor/briefing-markdown";
import { PlayBriefing } from "@/components/advisor/play-briefing";
import { AdvisorSourceBadge } from "@/components/advisor/source-badge";
import { getBriefing } from "@/lib/api/advisor";
import { DEMO_BRIEFING } from "@/lib/advisor-demo";
import { formatGeneratedAt, formatImpactUsd } from "@/lib/advisor-format";

export const metadata = { title: "Advisor · KubeHero" };
export const dynamic = "force-dynamic";

// The agentic face of KubeHero: the advisor service reads the last 24h of
// fleet telemetry and writes a briefing — prose, a spoken script, and a set
// of guarded CRDs. This page renders it; it never triggers any mutation.

export default async function AdvisorPage() {
  const live = await getBriefing();
  const briefing = live?.briefing ?? DEMO_BRIEFING;
  const totalImpact = briefing.actions.reduce((s, a) => s + a.impactMonthlyUsd, 0);

  return (
    <>
      <Topbar crumbs={[{ label: "advisor" }]} />
      <div className="px-5 py-6">
        {/* ── briefing header ─────────────────────────────────────────── */}
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div className="min-w-0 max-w-3xl">
            <div className="mb-2 flex flex-wrap items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <Sparkles className="h-3 w-3 text-[var(--color-accent)]" />
              /// daily briefing · 24h window · generated {formatGeneratedAt(briefing.generatedAtUnix)}
              <AdvisorSourceBadge source={briefing.source} />
            </div>
            <h1 className="text-[22px] font-medium leading-snug tracking-tight text-[var(--color-fg)]">
              {briefing.headline}
            </h1>
          </div>
          <PlayBriefing script={briefing.spokenScript} />
        </div>

        {/* ── two-column: prose · actions ─────────────────────────────── */}
        <div className="grid items-start gap-5 lg:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)]">
          {/* briefing body */}
          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                /// the briefing · what changed in the last 24h
              </span>
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                {briefing.id}
              </span>
            </div>
            <div className="px-4 py-4">
              <BriefingMarkdown markdown={briefing.markdown} />
            </div>
          </section>

          {/* proposed actions */}
          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
            <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-3">
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                /// proposed actions · {briefing.actions.length} guarded crds
              </span>
              <span className="font-mono text-[12px] tabular-nums text-[var(--color-signal)]">
                {formatImpactUsd(totalImpact)}
              </span>
            </div>
            {briefing.actions.length === 0 ? (
              <div className="px-4 py-12 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
                no actions proposed · the fleet looks healthy today
              </div>
            ) : (
              <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
                {briefing.actions.map((a, i) => (
                  <ActionCard key={a.id} action={a} rank={i + 1} />
                ))}
              </div>
            )}
            <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <span>advisor proposes · humans arm · operator executes</span>
              <span>kubehero advise --window 24h</span>
            </div>
          </section>
        </div>
      </div>
    </>
  );
}
