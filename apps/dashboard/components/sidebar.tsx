// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  AlertTriangle,
  Clock,
  Cpu,
  Gauge,
  Layers,
  Receipt,
  Settings,
  ShieldAlert,
  ShieldCheck,
  Zap,
} from "lucide-react";

type Item = {
  href: string;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  count?: number;
  tone?: string;
};

const items: Item[] = [
  { href: "/overview",   label: "Overview",     icon: Gauge },
  { href: "/fleet",      label: "Fleet",        icon: Layers,         count: 6 },
  { href: "/capacity",   label: "Capacity",     icon: Clock,          tone: "var(--color-warn)" },
  { href: "/waste",      label: "Waste",        icon: AlertTriangle,  tone: "var(--color-accent)" },
  { href: "/chargeback", label: "Chargeback",   icon: Receipt },
  { href: "/gpu",        label: "GPU panel",    icon: Cpu },
  { href: "/budgets",    label: "Budgets",      icon: Receipt,        count: 3 },
  { href: "/ceilings",   label: "Ceiling log",  icon: ShieldCheck,    tone: "var(--color-signal)" },
  { href: "/posture",    label: "Posture",      icon: ShieldAlert,    tone: "var(--color-warn)" },
  { href: "/settings",   label: "Settings",     icon: Settings },
];

export function Sidebar() {
  const path = usePathname();
  return (
    <aside className="sticky top-0 flex h-screen w-[220px] shrink-0 flex-col border-r border-[var(--color-line)] bg-[var(--color-bg-sunken)]">
      <div className="flex items-center gap-2 border-b border-[var(--color-line)] px-4 py-4 font-mono text-[11px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        <Zap className="h-3 w-3 text-[var(--color-accent)]" />
        <span>kubehero</span>
        <span>·</span>
        <span>dashboard</span>
      </div>

      <nav className="flex flex-col gap-0.5 p-2">
        {items.map((i) => {
          const active = path === i.href || path?.startsWith(i.href + "/");
          const Icon = i.icon;
          return (
            <Link
              key={i.href}
              href={i.href}
              className={`flex items-center gap-2.5 rounded-[2px] px-3 py-2 text-[13px] transition-colors ${
                active
                  ? "bg-[var(--color-bg-raised)] text-[var(--color-fg)]"
                  : "text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-raised)]/50 hover:text-[var(--color-fg)]"
              }`}
            >
              <Icon
                className="h-3.5 w-3.5"
                aria-hidden
              />
              <span className="flex-1">{i.label}</span>
              {i.count !== undefined && (
                <span
                  className="font-mono text-[10px] tabular-nums"
                  style={{ color: i.tone ?? "var(--color-fg-faint)" }}
                >
                  {i.count}
                </span>
              )}
            </Link>
          );
        })}
      </nav>

      {/* live feed footer */}
      <div className="mt-auto border-t border-[var(--color-line)] p-3">
        <div className="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          <span
            className="h-1.5 w-1.5"
            style={{ background: "var(--color-signal)" }}
            aria-hidden
          />
          control plane · connected
        </div>
        <div className="mt-2 font-mono text-[10px] text-[var(--color-fg-faint)]">
          502 nodes · 12,480 pods
        </div>
      </div>
    </aside>
  );
}
