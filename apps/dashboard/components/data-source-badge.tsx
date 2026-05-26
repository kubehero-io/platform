// SPDX-License-Identifier: BUSL-1.1
import { Activity, Database } from "lucide-react";

/* Tiny chip in any header that surfaces whether the page just rendered
   live data from the control-plane or fell back to demo data. */

export function DataSourceBadge({ source }: { source: "live" | "demo" }) {
  if (source === "live") {
    return (
      <span className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-signal)]">
        <Activity className="h-3 w-3" />
        live · control-plane
      </span>
    );
  }
  return (
    <span
      className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]"
      title="CONTROL_PLANE_URL is unset · serving demo data"
    >
      <Database className="h-3 w-3" />
      demo
    </span>
  );
}
