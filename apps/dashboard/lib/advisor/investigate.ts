// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// AdvisorService.Investigate(Stream) wire shapes (advisor.proto) and the
// mapping to what /ask renders. Pure + client-safe (the browser receives
// already-mapped events from /api/ask, but tests and the route share this).

import { arr, int64, num, str } from "@/lib/api/wire";
import type { AdvisorActionDTO, AdvisorActionKind, AdvisorRisk, AdvisorSource } from "@/lib/api/types";
import { safeLinkPath } from "@/lib/alerts/rules";

export type InvestigateStepJson = { tool?: string; inputJson?: string; summary?: string; durationMs?: number; error?: boolean };
export type EvidenceItemJson = { kind?: string; title?: string; detail?: string; linkPath?: string; query?: string };
export type ProposedActionJson = {
  id?: string;
  title?: string;
  impactMonthlyUsd?: number;
  risk?: string;
  kind?: string;
  target?: string;
  rationale?: string;
  crdYaml?: string;
  status?: string;
};
export type InvestigateResponseJson = {
  id?: string;
  answerMarkdown?: string;
  spokenSummary?: string;
  evidence?: EvidenceItemJson[];
  actions?: ProposedActionJson[];
  steps?: InvestigateStepJson[];
  source?: string;
  generatedAtUnix?: string | number;
};
export type InvestigateStreamResponseJson = { step?: InvestigateStepJson; progress?: string; result?: InvestigateResponseJson };

export type Step = { tool: string; input: string; summary: string; durationMs: number; error: boolean };
export type Evidence = { kind: string; title: string; detail: string; linkPath: string; query: string };
export type Investigation = {
  id: string;
  answerMarkdown: string;
  spokenSummary: string;
  evidence: Evidence[];
  actions: AdvisorActionDTO[];
  steps: Step[];
  source: AdvisorSource;
  generatedAtUnix: number;
};

/** Events /api/ask sends to the browser. */
export type AskEvent =
  | { event: "step"; data: Step }
  | { event: "progress"; data: { text: string } }
  | { event: "result"; data: Investigation }
  | { event: "error"; data: { code: string; message: string } }
  | { event: "end"; data: Record<string, never> };

const RISKS = new Set(["low", "medium", "high"]);
const KINDS = new Set(["rightsize.requests", "ceiling.arm", "nodepool.consolidate", "workload.investigate"]);
const SOURCES = new Set(["llm", "rules", "demo"]);

export function toStep(s: InvestigateStepJson): Step {
  return {
    tool: str(s.tool, "tool"),
    input: str(s.inputJson).slice(0, 2000),
    summary: str(s.summary),
    durationMs: num(s.durationMs),
    error: s.error === true,
  };
}

export function toAction(a: ProposedActionJson): AdvisorActionDTO {
  const risk = str(a.risk);
  const kind = str(a.kind);
  return {
    id: str(a.id) || `act-${Math.abs(hash(str(a.title)))}`,
    title: str(a.title),
    impactMonthlyUsd: num(a.impactMonthlyUsd),
    risk: (RISKS.has(risk) ? risk : "medium") as AdvisorRisk,
    kind: (KINDS.has(kind) ? kind : "workload.investigate") as AdvisorActionKind,
    target: str(a.target),
    rationale: str(a.rationale),
    crdYaml: str(a.crdYaml),
    status: str(a.status, "proposed"),
  };
}

export function toInvestigation(r: InvestigateResponseJson): Investigation {
  const source = str(r.source);
  return {
    id: str(r.id),
    answerMarkdown: str(r.answerMarkdown),
    spokenSummary: str(r.spokenSummary),
    evidence: arr(r.evidence).map((e) => ({
      kind: str(e.kind, "note"),
      title: str(e.title),
      detail: str(e.detail),
      linkPath: safeLinkPath(str(e.linkPath)),
      query: str(e.query),
    })),
    actions: arr(r.actions).map(toAction),
    steps: arr(r.steps).map(toStep),
    source: (SOURCES.has(source) ? source : "rules") as AdvisorSource,
    generatedAtUnix: int64(r.generatedAtUnix),
  };
}

/** One upstream stream message → the browser events it becomes. */
export function streamMessageToEvents(m: InvestigateStreamResponseJson): { event: string; data: unknown }[] {
  if (m.step) return [{ event: "step", data: toStep(m.step) }];
  if (typeof m.progress === "string") return [{ event: "progress", data: { text: m.progress.slice(0, 500) } }];
  if (m.result) return [{ event: "result", data: toInvestigation(m.result) }];
  return [];
}

function hash(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) h = (Math.imul(h, 31) + s.charCodeAt(i)) | 0;
  return h;
}

export const SUGGESTED_QUESTIONS = [
  "Why are checkout payments failing right now?",
  "What drove ml-inference spend up this week?",
  "Why did checkout get slower since yesterday?",
  "Which workloads are costing us the most in network egress?",
  "Why does cart keep getting OOM-killed?",
  "Where can we save $10k/month without risk?",
];
