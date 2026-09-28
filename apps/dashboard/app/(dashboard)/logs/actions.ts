// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use server";

// On-demand reads for the /logs client components: label values for
// autocomplete and "show context" around a line. Server actions are
// plain POSTs any client can send, so both re-check the session and
// validate every argument before calling the control plane.

import { listLogLabels, queryLogs } from "@/lib/api/logs";
import { currentSession } from "@/lib/api/guard";
import type { LogLine } from "@/lib/logs/types";
import { logqlSelector } from "@/lib/url";

const LABEL_RE = /^[A-Za-z_][A-Za-z0-9_.]{0,127}$/;

export async function labelValues(name: string, scope: string): Promise<string[]> {
  if (!(await currentSession())) return [];
  if (typeof name !== "string" || !LABEL_RE.test(name)) return [];
  const q = typeof scope === "string" && scope.length <= 4096 ? scope : "";
  const now = Date.now();
  const res = await listLogLabels({ name, query: q || undefined, range: { startMs: now - 6 * 3_600_000, endMs: now } });
  return res.data.values.slice(0, 200);
}

export type ContextResult = { before: LogLine[]; after: LogLine[]; error?: string };

export async function contextLines(line: { tsMs: number; labels: Record<string, string> }, n = 20): Promise<ContextResult> {
  if (!(await currentSession())) return { before: [], after: [], error: "sign in again" };
  const ns = String(line?.labels?.namespace ?? "");
  const pod = String(line?.labels?.pod ?? "");
  const ts = Number(line?.tsMs);
  if (!ns || !pod || !Number.isFinite(ts) || ns.length > 253 || pod.length > 253) {
    return { before: [], after: [], error: "context needs a namespace and pod label" };
  }
  const limit = Math.max(1, Math.min(50, Math.floor(n)));
  const selector = logqlSelector({ cluster: line.labels.cluster, namespace: ns, pod });
  const window = 30 * 60_000;
  const [before, after] = await Promise.all([
    queryLogs({ query: selector, range: { startMs: ts - window, endMs: ts }, limit, direction: "backward", stepMs: 60_000 }),
    queryLogs({ query: selector, range: { startMs: ts + 0.001, endMs: ts + window }, limit, direction: "forward", stepMs: 60_000 }),
  ]);
  const pick = (r: typeof before) => (r.data.kind === "streams" ? r.data.lines : []);
  return {
    before: pick(before).slice().reverse(),
    after: pick(after),
    error: before.queryError?.message ?? after.queryError?.message,
  };
}
