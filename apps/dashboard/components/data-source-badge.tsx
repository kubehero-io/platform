// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Chip in every view header saying where the numbers on screen came from.
// "Demo" always says WHY, so a degraded live dashboard is never mistaken
// for a healthy one and a demo is never mistaken for production data.

import { Activity, AlertTriangle, Database } from "lucide-react";
import type { DemoReason } from "@/lib/api/source";

const REASON: Record<DemoReason, { text: string; title: string; warn?: boolean }> = {
  unset: { text: "demo", title: "No control plane configured (CONTROL_PLANE_URL unset) · serving demo data" },
  error: { text: "demo · cp unreachable", title: "The control plane call failed · serving demo data", warn: true },
  empty: { text: "demo · no live data yet", title: "The control plane returned no data yet · serving demo data" },
  upstream: {
    text: "demo · from control plane",
    title: "The control plane itself is serving demo fixtures (no ClickHouse configured)",
  },
  forced: { text: "demo · preview", title: "Demo preview requested (?demo=1)" },
};

export function DataSourceBadge({
  source,
  reason,
  detail,
}: {
  source: "live" | "demo";
  reason?: DemoReason;
  detail?: string;
}) {
  if (source === "live") {
    return (
      <span className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-signal)]">
        <Activity className="h-3 w-3" aria-hidden />
        live · control-plane
      </span>
    );
  }
  const r = REASON[reason ?? "unset"];
  const Icon = r.warn ? AlertTriangle : Database;
  return (
    <span
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
      style={{ color: r.warn ? "var(--color-warn)" : "var(--color-fg-faint)" }}
      title={detail ? `${r.title} — ${detail}` : r.title}
    >
      <Icon className="h-3 w-3" aria-hidden />
      {r.text}
    </span>
  );
}
