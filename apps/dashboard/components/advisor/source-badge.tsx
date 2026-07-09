// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { Database, GitBranch, Sparkles } from "lucide-react";
import type { AdvisorSource } from "@/lib/api/types";

/* Advisor-specific sibling of DataSourceBadge: three states instead of two.
   `llm` gets the accent — it is the only place in the product where a model
   wrote the words on screen, and that should be visibly labelled. */

export function AdvisorSourceBadge({ source }: { source: AdvisorSource }) {
  if (source === "llm") {
    return (
      <span
        className="inline-flex items-center gap-1.5 border border-[var(--color-accent)]/40 bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-accent)]"
        title="This briefing was written by the advisor's language model"
      >
        <Sparkles className="h-3 w-3" />
        llm · generated
      </span>
    );
  }
  if (source === "rules") {
    return (
      <span
        className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-cool)]"
        title="This briefing was assembled by the advisor's deterministic rules engine"
      >
        <GitBranch className="h-3 w-3" />
        rules · deterministic
      </span>
    );
  }
  return (
    <span
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]"
      title="ADVISOR_URL is unset · serving demo briefing"
    >
      <Database className="h-3 w-3" />
      demo
    </span>
  );
}
