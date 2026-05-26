// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ShieldCheck } from "lucide-react";
import { Topbar } from "@/components/topbar";
import { DataSourceBadge } from "@/components/data-source-badge";
import { TableFilter } from "@/components/table-filter";
import { getAuditLog, type AuditEntry } from "@/lib/api/audit";

export const metadata = { title: "Ceiling log · KubeHero" };
export const dynamic = "force-dynamic";

function outcomeColor(o: AuditEntry["outcome"]) {
  switch (o) {
    case "applied":  return "var(--color-signal)";
    case "armed":    return "var(--color-cool)";
    case "cooldown": return "var(--color-warn)";
    case "reverted": return "var(--color-fg-dim)";
  }
}

type SearchParams = Promise<{ q?: string; outcome?: string; window?: string }>;

export default async function CeilingsPage({
  searchParams,
}: {
  searchParams: SearchParams;
}) {
  const sp = await searchParams;
  const win = sp.window === "24h" || sp.window === "7d" ? sp.window : "30d";
  // Push the outcome filter to the server (saves a roundtrip on long audit logs).
  const { entries: all, source } = await getAuditLog({ limit: 100, outcome: sp.outcome });

  const q = (sp.q ?? "").toLowerCase().trim();
  const entries = q
    ? all.filter(
        (e) =>
          e.policy.toLowerCase().includes(q) ||
          e.action.toLowerCase().includes(q) ||
          e.cluster.toLowerCase().includes(q),
      )
    : all;
  return (
    <>
      <Topbar crumbs={[{ label: "ceiling log" }]} />
      <div className="px-5 py-6">
        <div className="mb-6 flex flex-wrap items-end justify-between gap-3">
          <div>
            <div className="mb-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <ShieldCheck className="h-3 w-3 text-[var(--color-signal)]" />
              /// policy log · append-only · siem-exportable · {win}
            </div>
            <h1 className="text-[22px] font-medium tracking-tight text-[var(--color-fg)]">
              Every policy decision, every action, forever.
            </h1>
            <p className="mt-2 max-w-lg text-[14px] text-[var(--color-fg-dim)]">
              Applied actions are reversible via{" "}
              <code className="font-mono text-[12px] text-[var(--color-cool)]">
                kubehero undo &lt;audit-id&gt;
              </code>{" "}
              within the cooldown window (default 10m).
            </p>
          </div>
          <DataSourceBadge source={source} />
        </div>

        <TableFilter
          placeholder="search policy, cluster, or action…"
          facets={[
            {
              key: "outcome",
              label: "outcome",
              options: [
                { value: "applied", label: "applied" },
                { value: "armed", label: "armed" },
                { value: "cooldown", label: "cooldown" },
                { value: "reverted", label: "reverted" },
              ],
            },
          ]}
        />

        <div className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
          <div className="flex flex-col gap-[1px] bg-[var(--color-line)]">
            {entries.length === 0 && (
              <div className="bg-[var(--color-bg-raised)] px-4 py-8 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
                no audit entries match this filter ·{" "}
                <Link href="/ceilings" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                  clear filter →
                </Link>
              </div>
            )}
            {entries.map((e) => (
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
                <span className="w-48 shrink-0 truncate text-[var(--color-fg)]">
                  {e.policy}
                </span>
                <span className="min-w-[200px] flex-1 text-[var(--color-fg-dim)]">
                  {e.action}
                </span>
                <span className="shrink-0 text-[var(--color-fg-faint)]">
                  {e.cluster}
                </span>
                {e.effectK !== undefined && (
                  <span className="w-20 shrink-0 text-right tabular-nums text-[var(--color-signal)]">
                    −${e.effectK.toFixed(1)}k/mo
                  </span>
                )}
                <span className="w-28 shrink-0 text-right text-[var(--color-fg-faint)]">
                  {e.id}
                </span>
              </div>
            ))}
          </div>
          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>exporting · webhook + syslog + s3 · last 10m</span>
            <span className="text-[var(--color-cool)]">
              kubehero audit export
            </span>
          </div>
        </div>
      </div>
    </>
  );
}
