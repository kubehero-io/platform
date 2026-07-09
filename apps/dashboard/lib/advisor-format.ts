// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
//
// Pure display helpers for the Advisor page. DOM-free — tested under
// vitest's node environment.

import type { AdvisorActionKind, AdvisorRisk } from "./api/types";

/** "$18.2k/mo" above a thousand, "$680/mo" below. */
export function formatImpactUsd(monthly: number): string {
  if (!Number.isFinite(monthly) || monthly <= 0) return "$0/mo";
  if (monthly >= 1000) return `$${(monthly / 1000).toFixed(1)}k/mo`;
  return `$${Math.round(monthly)}/mo`;
}

/** Unix seconds → "2026-07-09 06:00 utc". Stable across server/client. */
export function formatGeneratedAt(unixSeconds: number): string {
  if (!Number.isFinite(unixSeconds) || unixSeconds <= 0) return "—";
  const d = new Date(unixSeconds * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${d.getUTCFullYear()}-${p(d.getUTCMonth() + 1)}-${p(d.getUTCDate())} ${p(d.getUTCHours())}:${p(d.getUTCMinutes())} utc`;
}

/**
 * Rough listen time for a spoken script at a given SpeechSynthesis rate.
 * ~165 words/min is a natural TTS baseline; rate scales it linearly.
 */
export function speechSeconds(script: string, rate = 1.05): number {
  const words = script.trim().split(/\s+/).filter(Boolean).length;
  if (words === 0) return 0;
  const wordsPerSecond = (165 * rate) / 60;
  return Math.max(1, Math.round(words / wordsPerSecond));
}

/** "1m 04s" / "48s" */
export function formatSeconds(total: number): string {
  const s = Math.max(0, Math.round(total));
  const m = Math.floor(s / 60);
  const r = s % 60;
  if (m === 0) return `${r}s`;
  return `${m}m ${String(r).padStart(2, "0")}s`;
}

export function riskTone(risk: AdvisorRisk): string {
  switch (risk) {
    case "low":
      return "var(--color-signal)";
    case "medium":
      return "var(--color-warn)";
    case "high":
      return "var(--color-accent)";
  }
}

export function kindLabel(kind: AdvisorActionKind): string {
  switch (kind) {
    case "rightsize.requests":
      return "rightsize";
    case "ceiling.arm":
      return "arm ceiling";
    case "nodepool.consolidate":
      return "consolidate";
    case "workload.investigate":
      return "investigate";
  }
}

/** "cluster/ns/workload" → parts (tolerates short targets). */
export function splitTarget(target: string): {
  cluster: string;
  namespace: string;
  workload: string;
} {
  const [cluster = "", namespace = "", ...rest] = target.split("/");
  return { cluster, namespace, workload: rest.join("/") };
}
