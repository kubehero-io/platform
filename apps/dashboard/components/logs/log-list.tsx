// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Virtualized log lines (@tanstack/react-virtual, dynamic row heights).
// A row expands in place to show every label (click → add or exclude it
// from the selector), a pretty-printed JSON body, and "show context"
// (±20 lines from the same pod via a server action). j/k or ↑/↓ move,
// ⏎ expands, c loads context.

import { useVirtualizer } from "@tanstack/react-virtual";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { memo, useCallback, useEffect, useRef, useState, useTransition } from "react";
import { ChevronRight, Loader2, Minus, Plus, TextSearch } from "lucide-react";
import Link from "next/link";
import { contextLines, type ContextResult } from "@/app/(dashboard)/logs/actions";
import { CopyButton } from "@/components/ui/copy-button";
import { LEVEL_COLOR, normalizeLevel } from "@/lib/chart/palette";
import { prettyJson } from "@/lib/logs/map";
import type { LogLine } from "@/lib/logs/types";
import { withMatcher } from "@/lib/logql/tokenize";
import { workloadHref } from "@/lib/url";

const pad = (n: number, w = 2) => String(n).padStart(w, "0");
export function fmtLineTime(ms: number, withDate = false): string {
  const d = new Date(ms);
  const t = `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}.${pad(d.getUTCMilliseconds(), 3)}`;
  return withDate ? `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())} ${t}` : t;
}

const HIDDEN_IN_SUMMARY = new Set(["cluster", "namespace", "workload", "pod", "container", "node", "level", "stream", "team", "app", "workload_kind"]);

export function LogList({
  lines,
  query,
  height = 560,
  emptyText = "No lines in this range.",
  follow = false,
}: {
  lines: LogLine[];
  query: string;
  height?: number;
  emptyText?: string;
  /** Live tail: keep the newest line in view. */
  follow?: boolean;
}) {
  const parentRef = useRef<HTMLDivElement>(null);
  const [expanded, setExpanded] = useState<Set<string>>(new Set());
  const [active, setActive] = useState(0);
  const [context, setContext] = useState<Record<string, ContextResult | "loading">>({});
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [, startNav] = useTransition();

  const virtualizer = useVirtualizer({
    count: lines.length,
    getScrollElement: () => parentRef.current,
    estimateSize: () => 26,
    overscan: 12,
    getItemKey: (i) => lines[i]?.id ?? i,
  });

  useEffect(() => {
    if (follow && lines.length > 0) virtualizer.scrollToIndex(lines.length - 1, { align: "end" });
  }, [follow, lines.length, virtualizer]);

  const toggle = useCallback((id: string) => {
    setExpanded((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  }, []);

  const filterBy = useCallback(
    (key: string, value: string, op: "=" | "!=") => {
      const next = new URLSearchParams(sp.toString());
      next.set("q", withMatcher(query, key, value, op));
      startNav(() => router.push(`${pathname}?${next.toString()}`, { scroll: false }));
    },
    [query, router, pathname, sp],
  );

  const loadContext = useCallback(async (l: LogLine) => {
    setContext((c) => ({ ...c, [l.id]: "loading" }));
    const r = await contextLines({ tsMs: l.tsMs, labels: l.labels }, 20).catch(() => ({ before: [], after: [], error: "context request failed" }));
    setContext((c) => ({ ...c, [l.id]: r }));
    setExpanded((s) => new Set(s).add(l.id));
  }, []);

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (lines.length === 0) return;
    const move = (d: number) => {
      e.preventDefault();
      const i = Math.max(0, Math.min(lines.length - 1, active + d));
      setActive(i);
      virtualizer.scrollToIndex(i, { align: "auto" });
    };
    if (e.key === "j" || e.key === "ArrowDown") move(1);
    else if (e.key === "k" || e.key === "ArrowUp") move(-1);
    else if (e.key === "Enter" || e.key === " ") {
      e.preventDefault();
      toggle(lines[active].id);
    } else if (e.key === "c") {
      e.preventDefault();
      void loadContext(lines[active]);
    } else if (e.key === "Home") move(-lines.length);
    else if (e.key === "End") move(lines.length);
  };

  if (lines.length === 0) {
    return (
      <div className="flex items-center justify-center gap-2 px-4 py-16 font-mono text-[12px] text-[var(--color-fg-faint)]">
        <TextSearch className="h-4 w-4" aria-hidden />
        {emptyText}
      </div>
    );
  }

  return (
    <div
      ref={parentRef}
      tabIndex={0}
      role="listbox"
      aria-label="log lines — j/k to move, enter to expand, c for context"
      aria-activedescendant={lines[active] ? `line-${lines[active].id}` : undefined}
      onKeyDown={onKeyDown}
      className="overflow-auto outline-none focus-visible:ring-1 focus-visible:ring-inset focus-visible:ring-[var(--color-cool)]"
      style={{ height }}
    >
      <div style={{ height: virtualizer.getTotalSize(), position: "relative", width: "100%" }}>
        {virtualizer.getVirtualItems().map((vi) => {
          const l = lines[vi.index];
          return (
            <div
              key={vi.key}
              ref={virtualizer.measureElement}
              data-index={vi.index}
              style={{ position: "absolute", top: 0, left: 0, width: "100%", transform: `translateY(${vi.start}px)` }}
            >
              <Row
                line={l}
                active={vi.index === active}
                expanded={expanded.has(l.id)}
                context={context[l.id]}
                onToggle={() => {
                  setActive(vi.index);
                  toggle(l.id);
                }}
                onFilter={filterBy}
                onContext={() => void loadContext(l)}
              />
            </div>
          );
        })}
      </div>
    </div>
  );
}

const Row = memo(function Row({
  line,
  active,
  expanded,
  context,
  onToggle,
  onFilter,
  onContext,
}: {
  line: LogLine;
  active: boolean;
  expanded: boolean;
  context?: ContextResult | "loading";
  onToggle: () => void;
  onFilter: (k: string, v: string, op: "=" | "!=") => void;
  onContext: () => void;
}) {
  const level = normalizeLevel(line.level);
  const color = LEVEL_COLOR[level];
  const who = [line.labels.namespace, line.labels.workload || line.labels.app].filter(Boolean).join("/");
  const pretty = expanded ? prettyJson(line.body) : null;
  return (
    <div
      id={`line-${line.id}`}
      role="option"
      aria-selected={active}
      aria-expanded={expanded}
      className={`border-b border-[var(--color-line)] ${active ? "bg-[var(--color-bg-sunken)]/70" : ""} ${expanded ? "bg-[var(--color-bg-sunken)]/50" : ""}`}
    >
      <button
        type="button"
        tabIndex={-1}
        onClick={onToggle}
        className="flex w-full items-start gap-2.5 px-3 py-[3px] text-left font-mono text-[12px] leading-[20px] hover:bg-[var(--color-bg-sunken)]/60"
      >
        <span className="mt-[9px] h-[3px] w-[3px] shrink-0 rounded-full" style={{ background: color }} aria-hidden />
        <ChevronRight className={`mt-[5px] h-2.5 w-2.5 shrink-0 text-[var(--color-fg-faint)] transition-transform ${expanded ? "rotate-90" : ""}`} aria-hidden />
        <span className="shrink-0 tabular-nums text-[var(--color-fg-faint)]">{fmtLineTime(line.tsMs)}</span>
        <span className="w-11 shrink-0 uppercase" style={{ color: level === "unknown" ? "var(--color-fg-faint)" : color }}>
          {level === "unknown" ? "—" : level}
        </span>
        <span className="hidden w-44 shrink-0 truncate text-[var(--color-fg-faint)] xl:block" title={who}>
          {who}
        </span>
        <span className={`min-w-0 flex-1 text-[var(--color-fg)] ${expanded ? "whitespace-pre-wrap break-all" : "truncate"}`}>{line.body}</span>
      </button>

      {expanded && (
        <div className="px-3 pb-3 pl-[46px]">
          {pretty && (
            <pre className="mb-2 max-h-72 overflow-auto border border-[var(--color-line)] bg-[var(--color-bg)] p-2 font-mono text-[11.5px] leading-relaxed text-[var(--color-fg-dim)]">{pretty}</pre>
          )}
          <div className="mb-2 flex flex-wrap gap-1.5">
            {Object.entries(line.labels)
              .sort(([a], [b]) => (HIDDEN_IN_SUMMARY.has(a) === HIDDEN_IN_SUMMARY.has(b) ? a.localeCompare(b) : HIDDEN_IN_SUMMARY.has(a) ? -1 : 1))
              .map(([k, v]) => (
                <span key={k} className="group inline-flex items-center border border-[var(--color-line-bright)] bg-[var(--color-bg)] font-mono text-[11px]">
                  <span className="px-1.5 py-0.5 text-[var(--color-syn-label)]">{k}</span>
                  <span className="max-w-[340px] truncate border-l border-[var(--color-line)] px-1.5 py-0.5 text-[var(--color-fg-dim)]" title={v}>
                    {v}
                  </span>
                  <button
                    type="button"
                    onClick={() => onFilter(k, v, "=")}
                    title={`filter to ${k}="${v}"`}
                    aria-label={`filter to ${k}=${v}`}
                    className="border-l border-[var(--color-line)] px-1 py-0.5 text-[var(--color-fg-faint)] hover:text-[var(--color-signal)]"
                  >
                    <Plus className="h-3 w-3" aria-hidden />
                  </button>
                  <button
                    type="button"
                    onClick={() => onFilter(k, v, "!=")}
                    title={`exclude ${k}="${v}"`}
                    aria-label={`exclude ${k}=${v}`}
                    className="border-l border-[var(--color-line)] px-1 py-0.5 text-[var(--color-fg-faint)] hover:text-[var(--color-warn)]"
                  >
                    <Minus className="h-3 w-3" aria-hidden />
                  </button>
                </span>
              ))}
          </div>
          <div className="flex flex-wrap items-center gap-3 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span className="normal-case tracking-normal">{fmtLineTime(line.tsMs, true)} utc · ns {line.tsNs}</span>
            <button type="button" onClick={onContext} className="inline-flex items-center gap-1 text-[var(--color-cool)] hover:text-[var(--color-fg)]">
              {context === "loading" && <Loader2 className="h-3 w-3 animate-spin" aria-hidden />}
              show context · ±20
            </button>
            <CopyButton text={line.body} label="copy line" />
            {line.labels.cluster && line.labels.namespace && line.labels.workload && (
              <Link href={workloadHref(line.labels.cluster, line.labels.namespace, line.labels.workload)} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                workload hub →
              </Link>
            )}
          </div>
          {context && context !== "loading" && <Context ctx={context} target={line} />}
        </div>
      )}
    </div>
  );
});

function Context({ ctx, target }: { ctx: ContextResult; target: LogLine }) {
  const row = (l: LogLine, dim: boolean) => (
    <div key={l.id} className={`flex gap-2.5 font-mono text-[11.5px] leading-[19px] ${dim ? "text-[var(--color-fg-faint)]" : "text-[var(--color-fg)]"}`}>
      <span className="shrink-0 tabular-nums">{fmtLineTime(l.tsMs)}</span>
      <span className="min-w-0 flex-1 truncate">{l.body}</span>
    </div>
  );
  return (
    <div className="mt-2 border border-[var(--color-line)] bg-[var(--color-bg)] p-2">
      <div className="mb-1 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        /// context · pod {target.labels.pod} · {ctx.before.length} before · {ctx.after.length} after
      </div>
      {ctx.error && <div className="font-mono text-[11px] text-[var(--color-warn)]">{ctx.error}</div>}
      {ctx.before.map((l) => row(l, true))}
      <div className="my-0.5 flex gap-2.5 border-l-2 border-[var(--color-cool)] pl-1.5 font-mono text-[11.5px] leading-[19px] text-[var(--color-fg)]">
        <span className="shrink-0 tabular-nums">{fmtLineTime(target.tsMs)}</span>
        <span className="min-w-0 flex-1 truncate">{target.body}</span>
      </div>
      {ctx.after.map((l) => row(l, true))}
    </div>
  );
}
