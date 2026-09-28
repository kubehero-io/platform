// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Sidebar: the IA from lib/nav.tsx, grouped. Badges and the footer come
// from /api/nav (components/nav-status.tsx) — live numbers, or demo
// numbers while the whole dashboard is in demo mode, or nothing at all.
// Below lg it collapses to an icon rail so tablets keep their width.

import Link from "next/link";
import { usePathname } from "next/navigation";
import { Zap } from "lucide-react";
import { useNavStatus } from "@/components/nav-status";
import { NAV, type NavBadge } from "@/lib/nav";

export function Sidebar() {
  const path = usePathname() ?? "";
  const status = useNavStatus();

  const badge = (b?: NavBadge): { text: string; tone: string; label: string } | null => {
    if (!b || !status) return null;
    if (b === "firing" && status.firing !== null && status.firing > 0) {
      return { text: String(status.firing), tone: "var(--color-accent)", label: `${status.firing} alerts firing` };
    }
    if (b === "recommendations" && status.recommendations !== null && status.recommendations > 0) {
      return { text: String(status.recommendations), tone: "var(--color-fg-faint)", label: `${status.recommendations} rightsizing recommendations` };
    }
    if (b === "clusters" && status.clusters.length > 0) {
      return { text: String(status.clusters.length), tone: "var(--color-fg-faint)", label: `${status.clusters.length} clusters` };
    }
    return null;
  };

  const health = status?.health;
  const healthTone = health === "live" ? "var(--color-signal)" : health === "unreachable" ? "var(--color-warn)" : "var(--color-fg-faint)";
  const healthText = health === "live" ? "control plane · connected" : health === "unreachable" ? "control plane · unreachable" : health === "demo" ? "demo · no control plane" : "connecting…";

  return (
    <aside className="sticky top-0 flex h-screen w-14 shrink-0 flex-col border-r border-[var(--color-line)] bg-[var(--color-bg-sunken)] lg:w-[220px]">
      <Link
        href="/overview"
        className="flex items-center gap-2 border-b border-[var(--color-line)] px-4 py-4 font-mono text-[11px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] hover:text-[var(--color-fg-dim)]"
        aria-label="KubeHero dashboard home"
      >
        <Zap className="h-3 w-3 shrink-0 text-[var(--color-accent)]" aria-hidden />
        <span className="hidden lg:inline">kubehero</span>
        <span className="hidden lg:inline">·</span>
        <span className="hidden lg:inline">dashboard</span>
      </Link>

      <nav aria-label="Main" className="flex min-h-0 flex-1 flex-col overflow-y-auto px-2 py-2">
        {NAV.map((g, gi) => (
          <div key={gi} className={gi > 0 ? "mt-3" : ""}>
            {g.label && (
              <div className="mb-1 hidden px-3 font-mono text-[9.5px] uppercase tracking-[0.16em] text-[var(--color-fg-faint)] lg:block">{g.label}</div>
            )}
            {!g.label && gi > 0 && <div className="mx-3 mb-2 border-t border-[var(--color-line)]" aria-hidden />}
            <ul className="flex flex-col gap-px">
              {g.items.map((i) => {
                const active = path === i.href || path.startsWith(i.href + "/") || (i.href === "/rightsizing" && path === "/waste");
                const Icon = i.icon;
                const b = badge(i.badge);
                return (
                  <li key={i.href}>
                    <Link
                      href={i.href}
                      aria-current={active ? "page" : undefined}
                      title={`${i.label}${i.key ? ` (g ${i.key})` : ""}`}
                      className={`relative flex items-center gap-2.5 rounded-[2px] px-3 py-[7px] text-[13px] transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:-outline-offset-1 focus-visible:outline-[var(--color-cool)] ${
                        active
                          ? "bg-[var(--color-bg-raised)] text-[var(--color-fg)]"
                          : "text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-raised)]/50 hover:text-[var(--color-fg)]"
                      }`}
                    >
                      {active && <span className="absolute inset-y-1.5 left-0 w-[2px] bg-[var(--color-fg)]" aria-hidden />}
                      <Icon className="h-3.5 w-3.5 shrink-0" aria-hidden />
                      <span className="hidden flex-1 truncate lg:inline">{i.label}</span>
                      {b && (
                        <span
                          className="absolute right-1 top-1 font-mono text-[9px] tabular-nums lg:static lg:text-[10px]"
                          style={{ color: b.tone }}
                          aria-label={b.label}
                          title={b.label}
                        >
                          {b.text}
                        </span>
                      )}
                    </Link>
                  </li>
                );
              })}
            </ul>
          </div>
        ))}
      </nav>

      <div className="border-t border-[var(--color-line)] p-3" role="status" aria-live="polite">
        <div className="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em]" style={{ color: healthTone }} title={healthText}>
          <span className="h-1.5 w-1.5 shrink-0" style={{ background: healthTone }} aria-hidden />
          <span className="hidden truncate lg:inline">{healthText}</span>
        </div>
        {status && status.clusters.length > 0 && (
          <div className="mt-2 hidden font-mono text-[10px] text-[var(--color-fg-faint)] lg:block">
            {status.clusters.length} cluster{status.clusters.length === 1 ? "" : "s"} · {status.nodes.toLocaleString("en-US")} nodes
            {status.fleetSource === "demo" ? " · demo" : ""}
          </div>
        )}
        <div className="mt-2 hidden font-mono text-[9.5px] text-[var(--color-fg-faint)] lg:block">
          <kbd className="border border-[var(--color-line)] px-1">?</kbd> shortcuts · <kbd className="border border-[var(--color-line)] px-1">⌘K</kbd> search
        </div>
      </div>
    </aside>
  );
}
