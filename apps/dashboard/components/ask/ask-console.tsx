// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// "Ask KubeHero": question in, live tool-call timeline out (SSE from
// /api/ask ← AdvisorService.InvestigateStream), then a grounded answer
// with evidence deep links and guarded proposals. Sessions persist in
// localStorage so an investigation survives a reload.

import { useCallback, useEffect, useEffectEvent, useRef, useState } from "react";
import { readLocalStorageJSON, useLocalStorageJSON, writeLocalStorage } from "@/lib/use-local-storage";
import Link from "next/link";
import { AnimatePresence, motion, useReducedMotion } from "motion/react";
import {
  ArrowUpRight,
  CircleAlert,
  CircleCheck,
  Clock3,
  CornerDownLeft,
  History,
  Loader2,
  MessageCircleQuestion,
  Square,
  Trash2,
  Wrench,
} from "lucide-react";
import { ActionCard } from "@/components/advisor/action-card";
import { BriefingMarkdown } from "@/components/advisor/briefing-markdown";
import { PlayBriefing } from "@/components/advisor/play-briefing";
import { AdvisorSourceBadge } from "@/components/advisor/source-badge";
import { CopyButton } from "@/components/ui/copy-button";
import { SUGGESTED_QUESTIONS, type Evidence, type Investigation, type Step } from "@/lib/advisor/investigate";
import { formatImpactUsd } from "@/lib/advisor-format";
import { readSse } from "@/lib/sse";

const HISTORY_KEY = "kh.ask.history.v1";
const HISTORY_MAX = 20;

type Entry = {
  id: string;
  question: string;
  context: string;
  askedAt: number;
  steps: Step[];
  notes: string[];
  result: Investigation | null;
  error: string | null;
};

const NO_HISTORY: Entry[] = [];

// Browser storage is user-editable: keep only entries whose shape the
// view can render.
function validateHistory(v: unknown): Entry[] | null {
  if (!Array.isArray(v)) return null;
  return (v as Partial<Entry>[])
    .filter(
      (e): e is Entry =>
        !!e &&
        typeof e === "object" &&
        typeof e.id === "string" &&
        typeof e.question === "string" &&
        typeof e.askedAt === "number" &&
        Array.isArray(e.steps) &&
        Array.isArray(e.notes),
    )
    .slice(0, HISTORY_MAX);
}

function updateHistory(fn: (xs: Entry[]) => Entry[]) {
  const next = fn(readLocalStorageJSON(HISTORY_KEY, NO_HISTORY, validateHistory)).slice(0, HISTORY_MAX);
  // Quota: drop the oldest half and retry once, then give up quietly.
  if (!writeLocalStorage(HISTORY_KEY, JSON.stringify(next))) {
    writeLocalStorage(HISTORY_KEY, JSON.stringify(next.slice(0, Math.floor(HISTORY_MAX / 2))));
  }
}

const EVIDENCE_TONE: Record<string, string> = {
  cost: "var(--color-signal)",
  anomaly: "var(--color-warn)",
  logs: "var(--color-cool)",
  profile: "var(--color-syn-fn)",
  network: "var(--color-warn)",
  event: "var(--color-accent)",
  rightsizing: "var(--color-signal)",
  alert: "var(--color-accent)",
};

export function AskConsole({ initialQuestion = "", context = "" }: { initialQuestion?: string; context?: string }) {
  const [question, setQuestion] = useState(initialQuestion);
  const [windowSel, setWindowSel] = useState<"1h" | "24h" | "7d">("24h");
  const [current, setCurrent] = useState<Entry | null>(null);
  const [running, setRunning] = useState(false);
  const [history] = useLocalStorageJSON(HISTORY_KEY, NO_HISTORY, validateHistory);
  const ctl = useRef<AbortController | null>(null);
  const autoRan = useRef(false);
  const reduce = useReducedMotion();

  useEffect(() => () => ctl.current?.abort(), []);

  const ask = useCallback(
    async (q: string) => {
      const text = q.trim();
      if (!text || running) return;
      ctl.current?.abort();
      const ac = new AbortController();
      ctl.current = ac;
      const entry: Entry = { id: `${Date.now()}`, question: text, context, askedAt: Date.now(), steps: [], notes: [], result: null, error: null };
      setCurrent(entry);
      setRunning(true);
      let live = entry;
      const update = (patch: Partial<Entry>) => {
        live = { ...live, ...patch };
        setCurrent(live);
      };
      try {
        const res = await fetch("/api/ask", {
          method: "POST",
          headers: { "Content-Type": "application/json", Accept: "text/event-stream" },
          body: JSON.stringify({ question: text, context, window: windowSel }),
          signal: ac.signal,
        });
        if (!res.ok) {
          const b = (await res.json().catch(() => null)) as { error?: { message?: string } } | null;
          update({ error: b?.error?.message ?? `request failed (HTTP ${res.status})` });
        } else {
          await readSse(
            res,
            (m) => {
              if (m.event === "step") update({ steps: [...live.steps, JSON.parse(m.data) as Step] });
              else if (m.event === "progress") update({ notes: [...live.notes, (JSON.parse(m.data) as { text: string }).text] });
              else if (m.event === "result") update({ result: JSON.parse(m.data) as Investigation });
              else if (m.event === "error") {
                const e = JSON.parse(m.data) as { code: string; message: string };
                update({ error: `${e.code}: ${e.message}` });
              }
            },
            ac.signal,
          );
          if (!live.result && !live.error) update({ error: "the investigation ended without an answer" });
        }
      } catch (e) {
        if (!ac.signal.aborted) update({ error: e instanceof Error ? e.message : "connection lost" });
        else update({ error: "stopped" });
      } finally {
        setRunning(false);
        if (live.result || live.error) {
          const done = live;
          updateHistory((h) => [done, ...h.filter((x) => x.question !== done.question)]);
        }
      }
    },
    [context, running, windowSel],
  );

  // Deep links (/ask?q=…) run once on arrival. Deferred a tick so React's
  // StrictMode mount → unmount → mount rehearsal cancels the timer rather
  // than aborting an in-flight request (the page is keyed by q, so a new
  // question remounts this component).
  const runInitial = useEffectEvent(() => {
    autoRan.current = true;
    void ask(initialQuestion);
  });
  useEffect(() => {
    if (!initialQuestion || autoRan.current) return;
    const t = setTimeout(runInitial, 0);
    return () => clearTimeout(t);
  }, [initialQuestion]);

  const r = current?.result ?? null;
  const totalImpact = r ? r.actions.reduce((s, a) => s + a.impactMonthlyUsd, 0) : 0;

  return (
    <div className="grid gap-5 xl:grid-cols-[minmax(0,1fr)_280px]">
      <div className="flex min-w-0 flex-col gap-4">
        {/* question box */}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void ask(question);
          }}
          className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] focus-within:border-[var(--color-cool)]"
        >
          {context && (
            <div className="flex items-center gap-2 border-b border-[var(--color-line)] px-3 py-1.5 font-mono text-[10.5px] text-[var(--color-fg-faint)]">
              context ·{" "}
              <Link href={context} className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
                {context}
              </Link>
            </div>
          )}
          <textarea
            value={question}
            onChange={(e) => setQuestion(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && !e.shiftKey) {
                e.preventDefault();
                void ask(question);
              }
            }}
            rows={2}
            maxLength={2000}
            data-search-input
            aria-label="Ask a question about your fleet"
            placeholder="Ask anything about cost, errors, latency, network or capacity…  e.g. why did checkout's spend jump last night?"
            className="block w-full resize-none bg-transparent px-3 py-3 text-[14px] leading-relaxed text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)]"
          />
          <div className="flex flex-wrap items-center justify-between gap-2 border-t border-[var(--color-line)] px-3 py-2">
            <div role="radiogroup" aria-label="time window" className="inline-flex gap-0.5 font-mono text-[10px] uppercase tracking-[0.12em]">
              {(["1h", "24h", "7d"] as const).map((w) => (
                <button
                  key={w}
                  type="button"
                  role="radio"
                  aria-checked={windowSel === w}
                  onClick={() => setWindowSel(w)}
                  className="px-2 py-1"
                  style={{ background: windowSel === w ? "var(--color-fg)" : "transparent", color: windowSel === w ? "var(--color-bg)" : "var(--color-fg-dim)" }}
                >
                  {w}
                </button>
              ))}
            </div>
            <div className="flex items-center gap-2">
              <span className="hidden font-mono text-[10px] text-[var(--color-fg-faint)] sm:inline">read-only · proposals only</span>
              {running ? (
                <button type="button" onClick={() => ctl.current?.abort()} className="btn-secondary !px-3 !py-1.5 !text-[12px]">
                  <Square className="h-3 w-3" aria-hidden /> stop
                </button>
              ) : (
                <button type="submit" disabled={!question.trim()} className="btn-primary !px-3 !py-1.5 !text-[12px] disabled:opacity-50">
                  ask <CornerDownLeft className="h-3.5 w-3.5" aria-hidden />
                </button>
              )}
            </div>
          </div>
        </form>

        {!current && (
          <div className="flex flex-wrap gap-2">
            {SUGGESTED_QUESTIONS.map((s) => (
              <button
                key={s}
                type="button"
                onClick={() => {
                  setQuestion(s);
                  void ask(s);
                }}
                className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-3 py-1.5 text-left text-[12.5px] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-fg)]"
              >
                {s}
              </button>
            ))}
          </div>
        )}

        {current && (
          <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]" aria-live="polite" aria-busy={running}>
            <header className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-line)] px-4 py-2.5">
              <span className="section-label">/// investigation · {current.steps.length} tool call{current.steps.length === 1 ? "" : "s"}</span>
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                {running ? (
                  <span className="inline-flex items-center gap-1.5 text-[var(--color-cool)]">
                    <Loader2 className="h-3 w-3 animate-spin" aria-hidden /> working
                  </span>
                ) : current.error ? (
                  "stopped"
                ) : (
                  `${(current.steps.reduce((s, x) => s + x.durationMs, 0) / 1000).toFixed(1)}s of tool time`
                )}
              </span>
            </header>
            <ol className="flex flex-col px-4 py-3">
              <AnimatePresence initial={false}>
                {current.notes.map((n, i) => (
                  <motion.li
                    key={`n${i}`}
                    initial={reduce ? false : { opacity: 0, y: 4 }}
                    animate={{ opacity: 1, y: 0 }}
                    className="flex items-center gap-2 py-0.5 font-mono text-[11px] text-[var(--color-fg-faint)]"
                  >
                    <Clock3 className="h-3 w-3 shrink-0" aria-hidden /> {n}
                  </motion.li>
                ))}
                {current.steps.map((s, i) => (
                  <motion.li
                    key={`s${i}`}
                    initial={reduce ? false : { opacity: 0, y: 4 }}
                    animate={{ opacity: 1, y: 0 }}
                    className="flex items-start gap-2 border-l border-[var(--color-line-bright)] py-1 pl-3"
                  >
                    {s.error ? (
                      <CircleAlert className="mt-0.5 h-3.5 w-3.5 shrink-0 text-[var(--color-danger)]" aria-hidden />
                    ) : (
                      <CircleCheck className="mt-0.5 h-3.5 w-3.5 shrink-0 text-[var(--color-signal)]" aria-hidden />
                    )}
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap items-baseline gap-2 font-mono text-[11.5px]">
                        <span className="inline-flex items-center gap-1 text-[var(--color-syn-fn)]">
                          <Wrench className="h-3 w-3" aria-hidden />
                          {s.tool}
                        </span>
                        <span className="tabular-nums text-[var(--color-fg-faint)]">{Math.round(s.durationMs)}ms</span>
                      </div>
                      <div className="text-[12.5px] leading-relaxed text-[var(--color-fg-dim)]">{s.summary}</div>
                      {s.input && s.input !== "{}" && <div className="truncate font-mono text-[10.5px] text-[var(--color-fg-faint)]" title={s.input}>{s.input}</div>}
                    </div>
                  </motion.li>
                ))}
              </AnimatePresence>
              {current.error && (
                <li role="alert" className="mt-2 font-mono text-[11.5px] text-[var(--color-danger)]">
                  {current.error}
                </li>
              )}
            </ol>
          </section>
        )}

        {r && (
          <>
            <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
              <header className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-line)] px-4 py-2.5">
                <span className="flex items-center gap-2">
                  <span className="section-label">/// answer</span>
                  <AdvisorSourceBadge source={r.source} />
                </span>
                <span className="flex items-center gap-3">
                  <CopyButton text={r.answerMarkdown} label="copy markdown" />
                  {r.spokenSummary && <PlayBriefing script={r.spokenSummary} />}
                </span>
              </header>
              <div className="px-4 py-4">
                <BriefingMarkdown markdown={r.answerMarkdown} />
              </div>
            </section>

            {r.evidence.length > 0 && (
              <section>
                <h2 className="section-label mb-2">/// evidence · {r.evidence.length}</h2>
                <div className="grid gap-2 md:grid-cols-2">
                  {r.evidence.map((e, i) => (
                    <EvidenceCard key={i} e={e} />
                  ))}
                </div>
              </section>
            )}

            {r.actions.length > 0 && (
              <section className="border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
                <header className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-2.5">
                  <span className="section-label">/// proposed actions · guarded crds</span>
                  {totalImpact > 0 && <span className="font-mono text-[12px] tabular-nums text-[var(--color-signal)]">{formatImpactUsd(totalImpact)}</span>}
                </header>
                <div className="flex flex-col gap-px bg-[var(--color-line)]">
                  {r.actions.map((a, i) => (
                    <ActionCard key={a.id} action={a} rank={i + 1} />
                  ))}
                </div>
              </section>
            )}
          </>
        )}
      </div>

      {/* history */}
      <aside className="flex flex-col gap-2">
        <div className="flex items-center justify-between">
          <h2 className="section-label inline-flex items-center gap-1.5">
            <History className="h-3 w-3" aria-hidden /> history · this browser
          </h2>
          {history.length > 0 && (
            <button
              type="button"
              onClick={() => writeLocalStorage(HISTORY_KEY, null)}
              aria-label="clear history"
              className="text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]"
            >
              <Trash2 className="h-3 w-3" aria-hidden />
            </button>
          )}
        </div>
        {history.length === 0 ? (
          <p className="font-mono text-[11px] leading-relaxed text-[var(--color-fg-faint)]">Past investigations appear here and reopen instantly.</p>
        ) : (
          <ul className="flex flex-col gap-px border border-[var(--color-line)] bg-[var(--color-line)]">
            {history.map((h) => (
              <li key={h.id}>
                <button
                  type="button"
                  onClick={() => {
                    setCurrent(h);
                    setQuestion(h.question);
                  }}
                  className={`flex w-full flex-col gap-0.5 bg-[var(--color-bg-raised)] px-3 py-2 text-left transition-colors hover:bg-[var(--color-bg-sunken)] ${current?.id === h.id ? "bg-[var(--color-bg-sunken)]" : ""}`}
                >
                  <span className="line-clamp-2 text-[12px] leading-snug text-[var(--color-fg-dim)]">{h.question}</span>
                  <span className="font-mono text-[10px] text-[var(--color-fg-faint)]">
                    {new Date(h.askedAt).toISOString().slice(5, 16).replace("T", " ")} utc{h.result ? ` · ${h.result.source}` : h.error ? " · failed" : ""}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
        <div className="mt-2 flex items-start gap-2 border border-[var(--color-line)] px-3 py-2 text-[11.5px] leading-relaxed text-[var(--color-fg-faint)]">
          <MessageCircleQuestion className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          The agent only reads: costs, logs, profiles, flows, alerts. Anything it proposes is a guarded CRD a human arms.
        </div>
      </aside>
    </div>
  );
}

function EvidenceCard({ e }: { e: Evidence }) {
  const tone = EVIDENCE_TONE[e.kind] ?? "var(--color-fg-dim)";
  const body = (
    <>
      <div className="flex items-center justify-between gap-2">
        <span className="inline-flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.14em]" style={{ color: tone }}>
          <span className="h-1.5 w-1.5" style={{ background: tone }} aria-hidden />
          {e.kind}
        </span>
        {e.linkPath && <ArrowUpRight className="h-3.5 w-3.5 text-[var(--color-fg-faint)] group-hover:text-[var(--color-cool)]" aria-hidden />}
      </div>
      <div className="font-mono text-[12.5px] text-[var(--color-fg)]">{e.title}</div>
      {e.detail && <div className="text-[12px] leading-snug text-[var(--color-fg-dim)]">{e.detail}</div>}
      {e.query && <code className="truncate font-mono text-[10.5px] text-[var(--color-fg-faint)]" title={e.query}>{e.query}</code>}
    </>
  );
  const cls = "group flex flex-col gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-3";
  return e.linkPath ? (
    <Link href={e.linkPath} className={`${cls} transition-colors hover:border-[var(--color-cool)]`}>
      {body}
    </Link>
  ) : (
    <div className={cls}>{body}</div>
  );
}
