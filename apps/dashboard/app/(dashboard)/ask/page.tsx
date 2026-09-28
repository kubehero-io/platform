// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// /ask — "Ask KubeHero": a read-only investigation agent over every
// signal (AdvisorService.InvestigateStream via /api/ask). ?q= runs a
// question on arrival (alerts, overview cards and workload hubs link
// here), ?context= scopes it to the page it was asked from.

import { MessageCircleQuestion } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { PageHeader } from "@/components/ui/page-header";
import { AdvisorSourceBadge } from "@/components/advisor/source-badge";
import { AskConsole } from "@/components/ask/ask-console";
import { isAdvisorLive } from "@/lib/api/advisor";
import { safeLinkPath } from "@/lib/alerts/rules";
import { param, type SearchParamsRecord } from "@/lib/url";

export const metadata = { title: "Ask · KubeHero" };
export const dynamic = "force-dynamic";

export default async function AskPage({ searchParams }: { searchParams: Promise<SearchParamsRecord> }) {
  const sp = await searchParams;
  const q = param(sp, "q", 2000);
  const context = safeLinkPath(param(sp, "context", 512));
  const live = isAdvisorLive();
  return (
    <>
      <Topbar crumbs={[{ label: "agents" }, { label: "ask" }]} range={false} />
      <div className="px-5 py-6">
        <PageHeader
          icon={MessageCircleQuestion}
          iconTone="var(--color-accent)"
          eyebrow={<>/// ask kubehero · investigation agent · read-only</>}
          title="Ask why. Get the evidence."
          sub="The agent pulls cost allocation, anomalies, logs and patterns, profiles, the network map and alerts, then answers with links to every fact it used."
          actions={
            live ? (
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-signal)]">advisor · connected</span>
            ) : (
              <AdvisorSourceBadge source="demo" />
            )
          }
        />
        <AskConsole key={`${q}|${context}`} initialQuestion={q} context={context} />
      </div>
    </>
  );
}
