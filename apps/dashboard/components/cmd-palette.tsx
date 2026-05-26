// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

"use client";

import {
  AlertTriangle,
  BookOpen,
  Cpu,
  CornerDownLeft,
  FileSearch,
  Layers,
  Receipt,
  ScanLine,
  Search,
  Settings as SettingsIcon,
  ShieldAlert,
  ShieldCheck,
  Terminal,
} from "lucide-react";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import { CLUSTERS } from "@/lib/fleet-data";

type Item = {
  id: string;
  label: string;
  kind: "nav" | "cluster" | "action" | "docs";
  hint?: string;
  href: string;
  external?: boolean;
  icon: React.ComponentType<{
    className?: string;
    style?: React.CSSProperties;
  }>;
};

const NAV: Item[] = [
  { id: "nav-fleet",      label: "Fleet",          kind: "nav", href: "/fleet",      icon: Layers },
  { id: "nav-waste",      label: "Waste",          kind: "nav", href: "/waste",      icon: AlertTriangle },
  { id: "nav-chargeback", label: "Chargeback",     kind: "nav", href: "/chargeback", icon: Receipt },
  { id: "nav-gpu",        label: "GPU panel",      kind: "nav", href: "/gpu",        icon: Cpu },
  { id: "nav-budgets",    label: "Budgets",        kind: "nav", href: "/budgets",    icon: Receipt },
  { id: "nav-ceilings",   label: "Ceiling log",    kind: "nav", href: "/ceilings",   icon: ShieldCheck },
  { id: "nav-posture",    label: "Posture",        kind: "nav", href: "/posture",    icon: ShieldAlert },
  { id: "nav-settings",   label: "Settings",       kind: "nav", href: "/settings",   icon: SettingsIcon },
];

const ACTIONS: Item[] = [
  { id: "act-scan",       label: "Run waste scan",                   kind: "action", href: "/waste",      icon: ScanLine,  hint: "kubehero scan --report waste" },
  { id: "act-rightsize",  label: "Apply rightsize plan",             kind: "action", href: "/waste",      icon: Terminal,  hint: "kubehero rightsize --apply" },
  { id: "act-arm",        label: "Arm a budget policy",              kind: "action", href: "/budgets",    icon: ShieldCheck, hint: "kubehero cap --arm <name>" },
  { id: "act-audit",      label: "Export audit log",                 kind: "action", href: "/settings",   icon: FileSearch, hint: "kubehero audit forward" },
];

const DOCS: Item[] = [
  { id: "doc-overview",    label: "Overview",                         kind: "docs", href: "https://kubehero.io/docs/overview",                       external: true, icon: BookOpen },
  { id: "doc-quickstart",  label: "Quickstart",                       kind: "docs", href: "https://kubehero.io/docs/quickstart",                     external: true, icon: BookOpen },
  { id: "doc-crd",         label: "CRD reference · BudgetPolicy",     kind: "docs", href: "https://kubehero.io/docs/crd-reference",                  external: true, icon: BookOpen },
  { id: "doc-chargeback",  label: "Chargeback — the label convention", kind: "docs", href: "https://kubehero.io/docs/chargeback",                    external: true, icon: BookOpen },
  { id: "doc-metrics",     label: "Metrics reference",                kind: "docs", href: "https://kubehero.io/docs/metrics-reference",              external: true, icon: BookOpen },
  { id: "doc-production",  label: "Production · HA + federation",     kind: "docs", href: "https://kubehero.io/docs/production",                     external: true, icon: BookOpen },
  { id: "doc-clouds",      label: "Cloud integrations (AWS · GCP · Azure)", kind: "docs", href: "https://kubehero.io/docs/integrations/clouds",     external: true, icon: BookOpen },
  { id: "doc-troubleshoot", label: "Troubleshooting",                 kind: "docs", href: "https://kubehero.io/docs/troubleshooting",                external: true, icon: BookOpen },
];

// Precompute cluster items — cheap.
const CLUSTER_ITEMS: Item[] = CLUSTERS.map((c) => ({
  id: `cluster-${c.id}`,
  label: c.name,
  kind: "cluster" as const,
  hint: `${c.cloud} · ${c.region} · ${c.nodes} nodes`,
  href: `/clusters/${c.id}`,
  icon: Layers,
}));

const ALL: Item[] = [...NAV, ...ACTIONS, ...CLUSTER_ITEMS, ...DOCS];

function matches(item: Item, q: string) {
  if (!q) return true;
  const needle = q.toLowerCase();
  return (
    item.label.toLowerCase().includes(needle) ||
    (item.hint?.toLowerCase().includes(needle) ?? false)
  );
}

function kindLabel(k: Item["kind"]) {
  return k === "nav"
    ? "navigate"
    : k === "cluster"
      ? "cluster"
      : k === "action"
        ? "action"
        : "docs";
}

export function CmdPalette() {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [cursor, setCursor] = useState(0);
  const router = useRouter();
  const inputRef = useRef<HTMLInputElement>(null);

  // cmd+k / ctrl+k
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // reset on open
  useEffect(() => {
    if (open) {
      setQ("");
      setCursor(0);
      setTimeout(() => inputRef.current?.focus(), 0);
    }
  }, [open]);

  const results = useMemo(() => ALL.filter((i) => matches(i, q)).slice(0, 20), [q]);
  const grouped = useMemo(() => {
    const out: { kind: Item["kind"]; items: Item[] }[] = [];
    for (const kind of ["nav", "action", "cluster", "docs"] as const) {
      const items = results.filter((r) => r.kind === kind);
      if (items.length) out.push({ kind, items });
    }
    return out;
  }, [results]);

  const go = useCallback(
    (item: Item) => {
      setOpen(false);
      if (item.external) {
        window.open(item.href, "_blank", "noopener,noreferrer");
      } else {
        router.push(item.href);
      }
    },
    [router],
  );

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(results.length - 1, c + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
    } else if (e.key === "Enter") {
      const item = results[cursor];
      if (item) go(item);
    }
  };

  return (
    <AnimatePresence>
      {open && (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.12 }}
          className="fixed inset-0 z-50 flex items-start justify-center bg-[var(--color-bg-sunken)]/85 px-4 pt-[14vh] backdrop-blur-sm"
          onClick={() => setOpen(false)}
        >
          <motion.div
            initial={{ y: -10, opacity: 0 }}
            animate={{ y: 0, opacity: 1 }}
            exit={{ y: -10, opacity: 0 }}
            transition={{ duration: 0.15 }}
            className="w-full max-w-[640px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] shadow-2xl"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="flex items-center gap-3 border-b border-[var(--color-line)] px-4 py-3">
              <Search className="h-4 w-4 text-[var(--color-fg-faint)]" aria-hidden />
              <input
                ref={inputRef}
                type="text"
                value={q}
                onChange={(e) => {
                  setQ(e.target.value);
                  setCursor(0);
                }}
                onKeyDown={onKey}
                placeholder="Search · ⌘K  ·  clusters, workloads, docs, actions"
                className="flex-1 bg-transparent font-mono text-[13px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)]"
              />
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                esc
              </span>
            </div>

            <div className="max-h-[56vh] overflow-y-auto py-1.5">
              {grouped.length === 0 ? (
                <div className="px-4 py-6 text-center font-mono text-[12px] text-[var(--color-fg-faint)]">
                  no matches
                </div>
              ) : (
                grouped.map(({ kind, items }) => (
                  <div key={kind} className="mt-1.5 first:mt-0">
                    <div className="px-4 pb-1 pt-1.5 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                      /// {kindLabel(kind)}
                    </div>
                    {items.map((item) => {
                      const globalIdx = results.findIndex((r) => r.id === item.id);
                      const active = globalIdx === cursor;
                      const Icon = item.icon;
                      return (
                        <button
                          key={item.id}
                          type="button"
                          onMouseEnter={() => setCursor(globalIdx)}
                          onClick={() => go(item)}
                          className={`flex w-full items-center gap-3 px-4 py-2 text-left transition-colors ${
                            active ? "bg-[var(--color-bg-sunken)]" : ""
                          }`}
                        >
                          <Icon
                            className="h-3.5 w-3.5 shrink-0"
                            style={{
                              color: active ? "var(--color-signal)" : "var(--color-fg-faint)",
                            }}
                          />
                          <span className="flex-1 truncate font-mono text-[12.5px] text-[var(--color-fg)]">
                            {item.label}
                          </span>
                          {item.hint && (
                            <span className="shrink-0 truncate font-mono text-[10px] text-[var(--color-fg-faint)]">
                              {item.hint}
                            </span>
                          )}
                          {active && (
                            <CornerDownLeft className="h-3 w-3 text-[var(--color-signal)]" />
                          )}
                        </button>
                      );
                    })}
                  </div>
                ))
              )}
            </div>

            <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <span>↑↓ navigate · enter to open</span>
              <span>KubeHero · ⌘K</span>
            </div>
          </motion.div>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
