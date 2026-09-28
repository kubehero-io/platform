// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// LogQL editor: a transparent <textarea> over a syntax-highlighted <pre>
// (identical font metrics, synced scroll), label-name/value autocomplete
// driven by lib/logql/tokenize.completionContext, recent queries in
// localStorage, and ⏎ / ⌘⏎ to run. The textarea stays the real input, so
// native editing, IME, undo and screen readers all just work.

import { useCallback, useId, useMemo, useRef, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { BookOpen, History, Play, X } from "lucide-react";
import { labelValues } from "@/app/(dashboard)/logs/actions";
import { applyCompletion, completionContext, tokenize, type CompletionContext, type TokenKind } from "@/lib/logql/tokenize";

const RECENT_KEY = "kh.logs.recent";
const RECENT_MAX = 20;

const COLOR: Partial<Record<TokenKind, string>> = {
  label: "var(--color-syn-label)",
  string: "var(--color-syn-string)",
  function: "var(--color-syn-fn)",
  keyword: "var(--color-syn-kw)",
  stage: "var(--color-syn-kw)",
  pipe: "var(--color-syn-pipe)",
  number: "var(--color-syn-num)",
  duration: "var(--color-syn-num)",
  matcher: "var(--color-fg-dim)",
  operator: "var(--color-fg-dim)",
  brace: "var(--color-fg-faint)",
  paren: "var(--color-fg-faint)",
  bracket: "var(--color-fg-faint)",
  comma: "var(--color-fg-faint)",
};

export const EXAMPLES: { label: string; query: string }[] = [
  { label: "errors, fleet-wide", query: '{level=~"error|fatal"}' },
  { label: "payments timeouts", query: '{namespace="shop", workload="payments"} |= "timeout"' },
  { label: "gateway 5xx (json)", query: '{namespace="edge"} | json | status >= 500' },
  { label: "error rate by workload", query: 'sum by (workload) (rate({level=~"error|fatal"}[5m]))' },
  { label: "top 5 chattiest pods", query: 'topk(5, sum by (pod) (count_over_time({cluster="eks-use1-prod"}[5m])))' },
  { label: "OOM messages", query: '{level="fatal"} |~ "(?i)out of memory"' },
];

function loadRecent(): string[] {
  try {
    const v = JSON.parse(localStorage.getItem(RECENT_KEY) ?? "[]") as unknown;
    return Array.isArray(v) ? v.filter((x): x is string => typeof x === "string").slice(0, RECENT_MAX) : [];
  } catch {
    return [];
  }
}

function saveRecent(q: string) {
  try {
    const next = [q, ...loadRecent().filter((x) => x !== q)].slice(0, RECENT_MAX);
    localStorage.setItem(RECENT_KEY, JSON.stringify(next));
  } catch {
    /* private mode / quota */
  }
}

export function QueryEditor({
  initial,
  labelNames,
  error,
  pending: externalPending,
}: {
  initial: string;
  labelNames: string[];
  error?: { message: string; pos?: number };
  pending?: boolean;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [value, setValue] = useState(initial);
  const [cursor, setCursor] = useState(initial.length);
  const [ctx, setCtx] = useState<CompletionContext | null>(null);
  const [options, setOptions] = useState<string[]>([]);
  const [active, setActive] = useState(0);
  const [menu, setMenu] = useState<"none" | "recent" | "examples">("none");
  const [recent, setRecent] = useState<string[]>([]);
  const taRef = useRef<HTMLTextAreaElement>(null);
  const preRef = useRef<HTMLPreElement>(null);
  const valueCache = useRef(new Map<string, string[]>());
  const listId = useId();

  // The page keys this component by the query, so navigation that changes
  // it from outside (click-to-filter, pattern filter) remounts fresh state.

  const tokens = useMemo(() => tokenize(value), [value]);

  const run = useCallback(
    (q: string) => {
      const query = q.trim();
      if (!query) return;
      saveRecent(query);
      const next = new URLSearchParams(sp.toString());
      next.set("q", query);
      router.push(`${pathname}?${next.toString()}`, { scroll: false });
      setCtx(null);
    },
    [router, pathname, sp],
  );

  // Autocomplete: recompute context on every edit / caret move.
  const refreshCompletion = useCallback(
    (v: string, pos: number) => {
      const c = completionContext(v, pos);
      setCtx(c);
      setActive(0);
      if (!c) {
        setOptions([]);
        return;
      }
      if (c.kind === "label-name") {
        const p = c.prefix.toLowerCase();
        setOptions(labelNames.filter((n) => n.toLowerCase().startsWith(p) && n !== c.prefix).slice(0, 12));
        return;
      }
      const key = c.label;
      const filter = (vals: string[]) => vals.filter((x) => x.toLowerCase().includes(c.prefix.toLowerCase()) && x !== c.prefix).slice(0, 12);
      const cached = valueCache.current.get(key);
      if (cached) {
        setOptions(filter(cached));
        return;
      }
      setOptions([]);
      labelValues(key, "")
        .then((vals) => {
          valueCache.current.set(key, vals);
          // Only apply if the context is still this label.
          const now = completionContext(taRef.current?.value ?? "", taRef.current?.selectionStart ?? 0);
          if (now && now.kind === "label-value" && now.label === key) setOptions(filter(vals));
        })
        .catch(() => {});
    },
    [labelNames],
  );

  const accept = (opt: string) => {
    if (!ctx) return;
    const { query, cursor: pos } = applyCompletion(value, ctx, opt);
    setValue(query);
    setCtx(null);
    setOptions([]);
    requestAnimationFrame(() => {
      const ta = taRef.current;
      if (!ta) return;
      ta.focus();
      ta.setSelectionRange(pos, pos);
      setCursor(pos);
      refreshCompletion(query, pos);
    });
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const open = ctx !== null && options.length > 0;
    if (open) {
      if (e.key === "ArrowDown") {
        e.preventDefault();
        setActive((a) => (a + 1) % options.length);
        return;
      }
      if (e.key === "ArrowUp") {
        e.preventDefault();
        setActive((a) => (a - 1 + options.length) % options.length);
        return;
      }
      if (e.key === "Tab" || (e.key === "Enter" && !e.metaKey && !e.ctrlKey)) {
        e.preventDefault();
        accept(options[active]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        setCtx(null);
        return;
      }
    }
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey || !e.shiftKey)) {
      e.preventDefault();
      run(value);
    }
    if (e.key === "Escape") {
      setMenu("none");
      (e.target as HTMLTextAreaElement).blur();
    }
  };

  const onChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    setValue(e.target.value);
    setCursor(e.target.selectionStart);
    refreshCompletion(e.target.value, e.target.selectionStart);
  };

  const syncScroll = () => {
    if (preRef.current && taRef.current) {
      preRef.current.scrollTop = taRef.current.scrollTop;
      preRef.current.scrollLeft = taRef.current.scrollLeft;
    }
  };

  const rows = Math.min(6, Math.max(1, value.split("\n").length));
  const open = ctx !== null && options.length > 0;
  const errCol = error?.pos !== undefined ? error.pos + 1 : undefined;

  return (
    <div className="relative">
      <div className="flex items-stretch gap-px border border-[var(--color-line-bright)] bg-[var(--color-line)] focus-within:border-[var(--color-cool)]">
        <span className="flex items-center bg-[var(--color-bg-sunken)] px-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          logql
        </span>
        <div className="relative min-w-0 flex-1 bg-[var(--color-bg-sunken)]">
          <pre
            ref={preRef}
            aria-hidden
            className="pointer-events-none absolute inset-0 overflow-hidden whitespace-pre-wrap break-all px-3 py-2 font-mono text-[13px] leading-[20px]"
          >
            {tokens.map((t, i) => (
              <span
                key={i}
                style={{
                  color: COLOR[t.kind] ?? "var(--color-fg)",
                  textDecoration: t.unterminated || (error?.pos !== undefined && t.start <= error.pos && error.pos < t.end) ? "underline wavy var(--color-danger)" : undefined,
                }}
              >
                {t.text}
              </span>
            ))}
            {value.endsWith("\n") ? " " : null}
          </pre>
          <textarea
            ref={taRef}
            value={value}
            rows={rows}
            onChange={onChange}
            onKeyDown={onKeyDown}
            onScroll={syncScroll}
            onClick={(e) => {
              const pos = (e.target as HTMLTextAreaElement).selectionStart;
              setCursor(pos);
              refreshCompletion(value, pos);
            }}
            onKeyUp={(e) => {
              if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(e.key)) {
                const pos = (e.target as HTMLTextAreaElement).selectionStart;
                if (pos !== cursor) {
                  setCursor(pos);
                  refreshCompletion(value, pos);
                }
              }
            }}
            onBlur={() => setTimeout(() => setCtx(null), 120)}
            spellCheck={false}
            autoCapitalize="off"
            autoComplete="off"
            autoCorrect="off"
            data-search-input
            aria-label="LogQL query"
            role="combobox"
            aria-expanded={open}
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={open ? `${listId}-${active}` : undefined}
            aria-invalid={!!error}
            placeholder='{namespace="shop"} |= "error"'
            className="relative block w-full resize-none bg-transparent px-3 py-2 font-mono text-[13px] leading-[20px] text-transparent caret-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] selection:bg-[var(--color-cool)]/30 selection:text-transparent"
          />
        </div>
        <div className="flex items-stretch gap-px">
          <MenuButton label="recent queries" icon={History} onClick={() => { setRecent(loadRecent()); setMenu((m) => (m === "recent" ? "none" : "recent")); }} active={menu === "recent"} />
          <MenuButton label="example queries" icon={BookOpen} onClick={() => setMenu((m) => (m === "examples" ? "none" : "examples"))} active={menu === "examples"} />
          <button
            type="button"
            onClick={() => run(value)}
            className="inline-flex items-center gap-1.5 bg-[var(--color-fg)] px-3 font-mono text-[11px] uppercase tracking-[0.12em] text-[var(--color-bg)] transition-colors hover:bg-white focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] disabled:opacity-60"
            disabled={externalPending}
            title="Run query (⏎)"
          >
            <Play className="h-3 w-3" aria-hidden />
            run
          </button>
        </div>
      </div>

      {open && (
        <ul
          id={listId}
          role="listbox"
          aria-label={ctx?.kind === "label-value" ? `values of ${ctx.label}` : "label names"}
          className="absolute left-[52px] top-full z-30 mt-1 max-h-64 w-[360px] max-w-[calc(100%-52px)] overflow-auto border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] py-1 shadow-xl"
        >
          <li className="px-3 pb-1 pt-0.5 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]" role="presentation">
            {ctx?.kind === "label-value" ? `/// ${ctx.label} values` : "/// labels"} · ⇥ to accept
          </li>
          {options.map((o, i) => (
            <li
              key={o}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              onMouseDown={(e) => {
                e.preventDefault();
                accept(o);
              }}
              onMouseEnter={() => setActive(i)}
              className={`cursor-pointer truncate px-3 py-1 font-mono text-[12px] ${i === active ? "bg-[var(--color-bg-sunken)] text-[var(--color-fg)]" : "text-[var(--color-fg-dim)]"}`}
            >
              {o}
            </li>
          ))}
        </ul>
      )}

      {menu !== "none" && (
        <div className="absolute right-0 top-full z-30 mt-1 w-[520px] max-w-full border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] py-1 shadow-xl">
          <div className="flex items-center justify-between px-3 pb-1 pt-0.5 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>/// {menu === "recent" ? "recent queries · this browser" : "examples"}</span>
            <button type="button" onClick={() => setMenu("none")} aria-label="close" className="hover:text-[var(--color-fg)]">
              <X className="h-3 w-3" aria-hidden />
            </button>
          </div>
          {(menu === "recent" ? recent.map((q) => ({ label: "", query: q })) : EXAMPLES).map((x) => (
            <button
              key={x.query}
              type="button"
              onClick={() => {
                setValue(x.query);
                setMenu("none");
                run(x.query);
              }}
              className="flex w-full flex-col items-start gap-0.5 px-3 py-1.5 text-left hover:bg-[var(--color-bg-sunken)] focus-visible:bg-[var(--color-bg-sunken)] focus-visible:outline-none"
            >
              {x.label && <span className="font-mono text-[10px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]">{x.label}</span>}
              <span className="w-full truncate font-mono text-[12px] text-[var(--color-fg-dim)]">{x.query}</span>
            </button>
          ))}
          {menu === "recent" && recent.length === 0 && (
            <div className="px-3 py-3 font-mono text-[11px] text-[var(--color-fg-faint)]">no recent queries yet — run one with ⏎</div>
          )}
        </div>
      )}

      <div className="mt-1.5 flex min-h-[18px] flex-wrap items-center justify-between gap-2 font-mono text-[10.5px]">
        {error ? (
          <span role="alert" className="text-[var(--color-danger)]">
            {errCol !== undefined ? `col ${errCol} · ` : ""}
            {error.message}
          </span>
        ) : (
          <span className="text-[var(--color-fg-faint)]">⏎ run · ⇧⏎ newline · ⇥ complete · / focus</span>
        )}
      </div>
    </div>
  );
}

function MenuButton({
  label,
  icon: Icon,
  onClick,
  active,
}: {
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  onClick: () => void;
  active: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      aria-expanded={active}
      title={label}
      className={`flex items-center bg-[var(--color-bg-sunken)] px-2.5 transition-colors hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:-outline-offset-1 focus-visible:outline-[var(--color-cool)] ${active ? "text-[var(--color-fg)]" : "text-[var(--color-fg-faint)]"}`}
    >
      <Icon className="h-3.5 w-3.5" aria-hidden />
    </button>
  );
}
