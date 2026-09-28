// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// ⌘K / Ctrl-K palette: every view (with its `g <key>` shortcut), live
// clusters from /api/nav, and two query actions that turn whatever you
// typed into a destination — "search logs for …" and "ask KubeHero …".

import {
  BookOpen,
  CornerDownLeft,
  Layers,
  MessageCircleQuestion,
  Search,
  ScrollText,
  Siren,
  Terminal,
} from "lucide-react";
import { useRouter } from "next/navigation";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import { useNavStatus } from "@/components/nav-status";
import { NAV } from "@/lib/nav";
import { logqlString } from "@/lib/url";

type Kind = "query" | "nav" | "cluster" | "action" | "docs";

type Item = {
  id: string;
  label: string;
  kind: Kind;
  hint?: string;
  keywords?: string;
  shortcut?: string;
  href: string;
  external?: boolean;
  icon: React.ComponentType<{ className?: string; style?: React.CSSProperties }>;
};

const NAV_ITEMS: Item[] = NAV.flatMap((g) =>
  g.items.map((i) => ({
    id: `nav-${i.href}`,
    label: i.label,
    kind: "nav" as const,
    hint: g.label || undefined,
    keywords: i.keywords,
    shortcut: i.key ? `g ${i.key}` : undefined,
    href: i.href,
    icon: i.icon,
  })),
);

const ACTIONS: Item[] = [
  { id: "act-errors", label: "Show errors across the fleet", kind: "action", href: `/logs?q=${encodeURIComponent('{level=~"error|fatal"}')}`, icon: ScrollText, hint: "logs", keywords: "errors fatal" },
  { id: "act-firing", label: "What is firing right now", kind: "action", href: "/alerts", icon: Siren, hint: "alerts", keywords: "alerts incidents" },
  { id: "act-new-rule", label: "Create an alert rule", kind: "action", href: "/alerts?tab=rules&rule=new", icon: Siren, hint: "alerts", keywords: "new rule" },
  { id: "act-rightsize", label: "Review high-confidence rightsizing", kind: "action", href: "/rightsizing?conf=high&direction=downsize", icon: Terminal, hint: "kubehero rightsize list", keywords: "waste savings" },
  { id: "act-idle", label: "Allocation with idle shared by cost", kind: "action", href: "/allocation?idle=weighted", icon: Terminal, hint: "cost", keywords: "idle opencost" },
  { id: "act-focus", label: "Export FinOps FOCUS (30d)", kind: "action", href: "/api/export/focus?window=30d&aggregate=workload", icon: Terminal, hint: "csv", keywords: "focus export csv finops" },
];

const DOCS: Item[] = [
  { id: "doc-quickstart", label: "Quickstart", kind: "docs", href: "https://kubehero.io/docs/quickstart", external: true, icon: BookOpen },
  { id: "doc-logql", label: "LogQL reference", kind: "docs", href: "https://kubehero.io/docs/logs", external: true, icon: BookOpen },
  { id: "doc-alerts", label: "Alert rules — every kind", kind: "docs", href: "https://kubehero.io/docs/alerts", external: true, icon: BookOpen },
  { id: "doc-crd", label: "CRD reference · RightsizingPolicy / BudgetPolicy", kind: "docs", href: "https://kubehero.io/docs/crd-reference", external: true, icon: BookOpen },
  { id: "doc-troubleshoot", label: "Troubleshooting", kind: "docs", href: "https://kubehero.io/docs/troubleshooting", external: true, icon: BookOpen },
];

function matches(item: Item, q: string) {
  if (!q) return true;
  const hay = `${item.label} ${item.hint ?? ""} ${item.keywords ?? ""}`.toLowerCase();
  return q
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean)
    .every((t) => hay.includes(t));
}

const KIND_LABEL: Record<Kind, string> = { query: "search", nav: "navigate", cluster: "clusters", action: "actions", docs: "docs" };

export function CmdPalette() {
  const [open, setOpen] = useState(false);
  const [q, setQ] = useState("");
  const [cursor, setCursor] = useState(0);
  const router = useRouter();
  const inputRef = useRef<HTMLInputElement>(null);
  const status = useNavStatus();
  const reduce = useReducedMotion();

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
      if (e.key === "Escape") setOpen(false);
    };
    const onOpen = () => setOpen(true);
    window.addEventListener("keydown", onKey);
    window.addEventListener("kh:open-palette", onOpen);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("kh:open-palette", onOpen);
    };
  }, []);

  useEffect(() => {
    if (!open) return;
    const t = setTimeout(() => inputRef.current?.focus(), 0);
    return () => clearTimeout(t);
  }, [open]);

  const clusters: Item[] = useMemo(
    () =>
      (status?.clusters ?? []).map((c) => ({
        id: `cluster-${c.id}`,
        label: c.name,
        kind: "cluster" as const,
        hint: `${c.cloud} · ${c.region} · ${c.nodes} nodes`,
        href: `/clusters/${encodeURIComponent(c.id)}`,
        icon: Layers,
      })),
    [status],
  );

  const results = useMemo(() => {
    const text = q.trim();
    const dynamic: Item[] = text
      ? [
          {
            id: "q-ask",
            label: `Ask KubeHero: “${text.slice(0, 80)}”`,
            kind: "query",
            href: `/ask?q=${encodeURIComponent(text)}`,
            icon: MessageCircleQuestion,
            hint: "investigate",
          },
          {
            id: "q-logs",
            label: `Search logs for “${text.slice(0, 60)}”`,
            kind: "query",
            href: `/logs?q=${encodeURIComponent(text.startsWith("{") ? text : `{level=~".+"} |= ${logqlString(text)}`)}`,
            icon: ScrollText,
            hint: text.startsWith("{") ? "run as LogQL" : "line contains",
          },
        ]
      : [];
    return [...NAV_ITEMS, ...ACTIONS, ...clusters, ...DOCS].filter((i) => matches(i, text)).slice(0, 24).concat(dynamic);
  }, [q, clusters]);

  const grouped = useMemo(() => {
    const out: { kind: Kind; items: Item[] }[] = [];
    for (const kind of ["nav", "action", "cluster", "docs", "query"] as const) {
      const items = results.filter((r) => r.kind === kind);
      if (items.length) out.push({ kind, items });
    }
    return out;
  }, [results]);
  const flat = useMemo(() => grouped.flatMap((g) => g.items), [grouped]);

  const go = useCallback(
    (item: Item) => {
      setOpen(false);
      setQ("");
      setCursor(0);
      if (item.external) window.open(item.href, "_blank", "noopener,noreferrer");
      else if (item.href.startsWith("/api/")) window.location.href = item.href;
      else router.push(item.href);
    },
    [router],
  );

  const onKey = (e: React.KeyboardEvent) => {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setCursor((c) => Math.min(flat.length - 1, c + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
    } else if (e.key === "Enter") {
      const item = flat[cursor];
      if (item) go(item);
    }
  };

  const activeId = flat[cursor] ? `cmd-${flat[cursor].id}` : undefined;

  return (
    <AnimatePresence>
      {open && (
        <motion.div
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: reduce ? 0 : 0.12 }}
          className="fixed inset-0 z-50 flex items-start justify-center bg-[var(--color-bg-sunken)]/85 px-4 pt-[14vh] backdrop-blur-sm"
          onClick={() => setOpen(false)}
        >
          <motion.div
            role="dialog"
            aria-modal="true"
            aria-label="Command palette"
            initial={reduce ? { opacity: 0 } : { y: -10, opacity: 0 }}
            animate={reduce ? { opacity: 1 } : { y: 0, opacity: 1 }}
            exit={reduce ? { opacity: 0 } : { y: -10, opacity: 0 }}
            transition={{ duration: reduce ? 0 : 0.15 }}
            className="w-full max-w-[680px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] shadow-2xl"
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
                role="combobox"
                aria-expanded="true"
                aria-controls="cmd-results"
                aria-activedescendant={activeId}
                aria-label="Search views, clusters, actions — or type a question"
                placeholder="Jump to a view, a cluster, an action — or type a question or a log search…"
                className="flex-1 bg-transparent font-mono text-[13px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)]"
              />
              <kbd className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">esc</kbd>
            </div>

            <div id="cmd-results" role="listbox" className="max-h-[56vh] overflow-y-auto py-1.5">
              {grouped.length === 0 ? (
                <div className="px-4 py-6 text-center font-mono text-[12px] text-[var(--color-fg-faint)]">no matches</div>
              ) : (
                grouped.map(({ kind, items }) => (
                  <div key={kind} className="mt-1.5 first:mt-0" role="group" aria-label={KIND_LABEL[kind]}>
                    <div className="px-4 pb-1 pt-1.5 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">/// {KIND_LABEL[kind]}</div>
                    {items.map((item) => {
                      const idx = flat.indexOf(item);
                      const active = idx === cursor;
                      const Icon = item.icon;
                      return (
                        <button
                          key={item.id}
                          id={`cmd-${item.id}`}
                          role="option"
                          aria-selected={active}
                          type="button"
                          onMouseEnter={() => setCursor(idx)}
                          onClick={() => go(item)}
                          className={`flex w-full items-center gap-3 px-4 py-2 text-left transition-colors ${active ? "bg-[var(--color-bg-sunken)]" : ""}`}
                        >
                          <Icon className="h-3.5 w-3.5 shrink-0" style={{ color: active ? "var(--color-signal)" : "var(--color-fg-faint)" }} aria-hidden />
                          <span className="flex-1 truncate font-mono text-[12.5px] text-[var(--color-fg)]">{item.label}</span>
                          {item.hint && <span className="shrink-0 truncate font-mono text-[10px] text-[var(--color-fg-faint)]">{item.hint}</span>}
                          {item.shortcut && (
                            <kbd className="shrink-0 border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-1 font-mono text-[9.5px] text-[var(--color-fg-faint)]">{item.shortcut}</kbd>
                          )}
                          {active && <CornerDownLeft className="h-3 w-3 text-[var(--color-signal)]" aria-hidden />}
                        </button>
                      );
                    })}
                  </div>
                ))
              )}
            </div>

            <div className="flex items-center justify-between border-t border-[var(--color-line)] px-4 py-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <span>↑↓ move · ⏎ open · g+key jumps from anywhere · ? all shortcuts</span>
              <span>⌘K</span>
            </div>
          </motion.div>
        </motion.div>
      )}
    </AnimatePresence>
  );
}
