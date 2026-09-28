// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Alert rule editor: every kind from alerts.proto, per-kind query help
// with one-click examples, inline validation (the same rules the server
// action re-runs), a live "Test" that evaluates the draft once without
// persisting or notifying (TestAlertRule), and save/delete via server
// actions. Admin-only controls are disabled — not hidden — for other
// roles, with the reason on screen.

import { useMemo, useState, useTransition } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { FlaskConical, Loader2, Lock, Save, Trash2 } from "lucide-react";
import { removeRule, saveRule, testRule } from "@/app/(dashboard)/alerts/actions";
import { useToast } from "@/components/toast";
import { Chip } from "@/components/ui/chip";
import { draftFromRule, KIND_HELP, validateDraft } from "@/lib/alerts/rules";
import { validateRuleQuery } from "@/lib/alerts/selector";
import { OPS, RULE_KINDS, SEVERITIES, type AlertRule, type RuleDraft, type TestResult } from "@/lib/alerts/types";
import { tokenize, type TokenKind } from "@/lib/logql/tokenize";

const SYN: Partial<Record<TokenKind, string>> = {
  label: "var(--color-syn-label)",
  string: "var(--color-syn-string)",
  function: "var(--color-syn-fn)",
  keyword: "var(--color-syn-kw)",
  pipe: "var(--color-syn-pipe)",
  number: "var(--color-syn-num)",
  duration: "var(--color-syn-num)",
};

const input =
  "w-full border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1.5 font-mono text-[12px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] focus:border-[var(--color-cool)] aria-[invalid=true]:border-[var(--color-danger)]";

export function RuleEditor({ rule, canAdmin }: { rule?: AlertRule; canAdmin: boolean }) {
  const [d, setD] = useState<RuleDraft>(() => draftFromRule(rule));
  const [touched, setTouched] = useState(false);
  const [test, setTest] = useState<(TestResult & { demo: boolean }) | null>(null);
  const [serverError, setServerError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const { toast } = useToast();
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();

  const errors = useMemo(() => {
    const e = validateDraft(d);
    const q = validateRuleQuery(d.kind, d.query);
    return q ? { ...e, query: q } : e;
  }, [d]);
  const valid = Object.keys(errors).length === 0;
  const help = KIND_HELP[d.kind];
  const set = <K extends keyof RuleDraft>(k: K, v: RuleDraft[K]) => setD((x) => ({ ...x, [k]: v }));
  const err = (k: keyof RuleDraft) => (touched || d[k] !== draftFromRule(rule)[k] ? errors[k] : undefined);

  const close = () => {
    const next = new URLSearchParams(sp.toString());
    next.delete("rule");
    router.replace(`${pathname}?${next.toString()}`, { scroll: false });
  };

  const onTest = () => {
    setTouched(true);
    setServerError(null);
    if (!valid) return;
    start(async () => {
      const r = await testRule(d);
      if (r.ok && r.data) setTest(r.data);
      else if (!r.ok) setServerError(r.error.message);
    });
  };

  const onSave = () => {
    setTouched(true);
    setServerError(null);
    if (!valid) return;
    start(async () => {
      const r = await saveRule(d);
      if (!r.ok) {
        setServerError(r.error.message);
        toast({ tone: "err", title: "Rule not saved", sub: r.error.message });
        return;
      }
      toast({ tone: r.demo ? "info" : "ok", title: `${d.name} ${rule ? "updated" : "created"}`, sub: r.note ?? "evaluated every " + d.evalInterval });
      close();
    });
  };

  const onDelete = () => {
    if (!rule) return;
    start(async () => {
      const r = await removeRule(rule.id);
      if (!r.ok) {
        setServerError(r.error.message);
        return;
      }
      toast({ tone: "info", title: `${rule.name} deleted`, sub: r.note });
      close();
    });
  };

  return (
    <form
      className="flex flex-col gap-4 px-5 py-4"
      onSubmit={(e) => {
        e.preventDefault();
        onSave();
      }}
      noValidate
    >
      {!canAdmin && (
        <div className="flex items-start gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-2 font-mono text-[11px] text-[var(--color-fg-dim)]">
          <Lock className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          Read-only: creating, editing and deleting alert rules needs the admin role. You can still test a rule.
        </div>
      )}

      <Field label="name" error={err("name")}>
        <input className={input} value={d.name} onChange={(e) => set("name", e.target.value)} maxLength={128} aria-invalid={!!err("name")} placeholder="Payments timeouts" />
      </Field>
      <Field label="description">
        <input className={input} value={d.description} onChange={(e) => set("description", e.target.value)} maxLength={512} placeholder="What this catches and why it matters" />
      </Field>

      <Field label="kind" group>
        <div role="radiogroup" aria-label="rule kind" className="flex flex-wrap gap-1">
          {RULE_KINDS.map((k) => (
            <button
              key={k}
              type="button"
              role="radio"
              aria-checked={d.kind === k}
              onClick={() => {
                set("kind", k);
                setTest(null);
              }}
              className={`border px-2 py-1 font-mono text-[10.5px] uppercase tracking-[0.12em] transition-colors ${
                d.kind === k ? "border-[var(--color-fg)] bg-[var(--color-fg)] text-[var(--color-bg)]" : "border-[var(--color-line-bright)] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
              }`}
            >
              {k}
            </button>
          ))}
        </div>
      </Field>

      <Field label="query" error={err("query")} hint={help.title} group>
        <div className="relative">
          {d.kind === "logs" && (
            <pre aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden whitespace-pre-wrap break-all px-2.5 py-1.5 font-mono text-[12px] leading-[18px]">
              {tokenize(d.query).map((t, i) => (
                <span key={i} style={{ color: SYN[t.kind] ?? "var(--color-fg)" }}>
                  {t.text}
                </span>
              ))}
            </pre>
          )}
          <textarea
            className={`${input} relative min-h-[64px] resize-y leading-[18px] ${d.kind === "logs" ? "bg-transparent text-transparent caret-[var(--color-fg)]" : ""}`}
            style={d.kind === "logs" ? { background: "transparent" } : undefined}
            value={d.query}
            onChange={(e) => set("query", e.target.value)}
            spellCheck={false}
            rows={3}
            aria-label="rule query"
            aria-invalid={!!err("query")}
            placeholder={help.grammar}
          />
        </div>
        <div className="mt-1.5 font-mono text-[10.5px] text-[var(--color-fg-faint)]">
          grammar · <span className="text-[var(--color-fg-dim)]">{help.grammar}</span>
        </div>
        <div className="mt-1.5 flex flex-wrap gap-1.5">
          {help.examples.map((x) => (
            <button
              key={x.q}
              type="button"
              onClick={() => setD((cur) => ({ ...cur, query: x.q, op: x.op, threshold: String(x.threshold) }))}
              className="border border-[var(--color-line)] bg-[var(--color-bg)] px-2 py-0.5 text-left font-mono text-[10.5px] text-[var(--color-fg-dim)] hover:border-[var(--color-cool)] hover:text-[var(--color-cool)]"
              title={x.q}
            >
              e.g. {x.note}
            </button>
          ))}
        </div>
      </Field>

      <div className="grid grid-cols-[88px_1fr_1fr] gap-3">
        <Field label="op" error={err("op")}>
          <select className={input} value={d.op} onChange={(e) => set("op", e.target.value as RuleDraft["op"])}>
            {OPS.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
        </Field>
        <Field label="threshold" error={err("threshold")}>
          <input className={input} inputMode="decimal" value={d.threshold} onChange={(e) => set("threshold", e.target.value)} aria-invalid={!!err("threshold")} placeholder="100" />
        </Field>
        <Field label="pending for" error={err("pendingFor")} hint="condition must hold this long">
          <input className={input} value={d.pendingFor} onChange={(e) => set("pendingFor", e.target.value)} aria-invalid={!!err("pendingFor")} placeholder="5m" />
        </Field>
      </div>

      <div className="grid grid-cols-2 gap-3">
        <Field label="severity">
          <select className={input} value={d.severity} onChange={(e) => set("severity", e.target.value as RuleDraft["severity"])}>
            {SEVERITIES.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>
        </Field>
        <Field label="evaluate every" error={err("evalInterval")}>
          <input className={input} value={d.evalInterval} onChange={(e) => set("evalInterval", e.target.value)} aria-invalid={!!err("evalInterval")} placeholder="1m" />
        </Field>
      </div>

      <Field label="channels" error={err("channels")} hint="slack:// pagerduty:// opsgenie:// teams:// webhook+https:// alertmanager+https:// discord+https://">
        <textarea className={`${input} min-h-[52px] resize-y`} value={d.channels} onChange={(e) => set("channels", e.target.value)} rows={2} spellCheck={false} aria-invalid={!!err("channels")} placeholder="slack://ops-oncall" />
      </Field>
      <Field label="summary annotation" error={err("summary")} hint="{{ $value }} and {{ $labels.name }} are templated">
        <input className={input} value={d.summary} onChange={(e) => set("summary", e.target.value)} placeholder="{{ $labels.workload }} logged {{ $value }} errors in 5m" />
      </Field>
      <div className="grid grid-cols-2 gap-3">
        <Field label="labels" error={err("labels")} hint="key=value, one per line">
          <textarea className={`${input} min-h-[52px] resize-y`} value={d.labels} onChange={(e) => set("labels", e.target.value)} rows={2} spellCheck={false} aria-invalid={!!err("labels")} placeholder="team=payments" />
        </Field>
        <Field label="runbook url" error={err("runbookUrl")}>
          <input className={input} value={d.runbookUrl} onChange={(e) => set("runbookUrl", e.target.value)} aria-invalid={!!err("runbookUrl")} placeholder="https://wiki/runbooks/…" />
        </Field>
      </div>
      <label className="inline-flex items-center gap-2 font-mono text-[11px] text-[var(--color-fg-dim)]">
        <input type="checkbox" checked={d.enabled} onChange={(e) => set("enabled", e.target.checked)} className="accent-[var(--color-signal)]" />
        enabled
      </label>

      {serverError && (
        <div role="alert" className="border border-[var(--color-danger)]/50 bg-[var(--color-bg-sunken)] px-3 py-2 font-mono text-[11px] text-[var(--color-danger)]">
          {serverError}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-[var(--color-line)] pt-4">
        <button type="button" onClick={onTest} disabled={pending} className="btn-secondary !px-3 !py-1.5 !text-[12px]">
          {pending ? <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden /> : <FlaskConical className="h-3.5 w-3.5" aria-hidden />}
          test now
        </button>
        <button type="submit" disabled={pending || !canAdmin} className="btn-primary !px-3 !py-1.5 !text-[12px] disabled:cursor-not-allowed disabled:opacity-50" title={!canAdmin ? "admin role required" : undefined}>
          <Save className="h-3.5 w-3.5" aria-hidden />
          {rule ? "save changes" : "create rule"}
        </button>
        {rule && canAdmin && (
          <span className="ml-auto inline-flex items-center gap-2">
            {confirmDelete ? (
              <>
                <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">delete “{rule.name}”?</span>
                <button type="button" onClick={onDelete} className="font-mono text-[11px] text-[var(--color-danger)] hover:underline">
                  yes, delete
                </button>
                <button type="button" onClick={() => setConfirmDelete(false)} className="font-mono text-[11px] text-[var(--color-fg-dim)] hover:underline">
                  cancel
                </button>
              </>
            ) : (
              <button type="button" onClick={() => setConfirmDelete(true)} className="inline-flex items-center gap-1 font-mono text-[11px] text-[var(--color-fg-faint)] hover:text-[var(--color-danger)]">
                <Trash2 className="h-3 w-3" aria-hidden /> delete
              </button>
            )}
          </span>
        )}
      </div>

      {test && <TestResults r={test} op={d.op} threshold={d.threshold} />}
    </form>
  );
}

function TestResults({ r, op, threshold }: { r: TestResult & { demo: boolean }; op: string; threshold: string }) {
  const fired = r.results.filter((x) => x.triggered).length;
  return (
    <section aria-live="polite" className="border border-[var(--color-line)] bg-[var(--color-bg-sunken)]">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-line)] px-3 py-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        <span>
          /// test · {r.results.length} series · <span style={{ color: fired > 0 ? "var(--color-accent)" : "var(--color-signal)" }}>{fired} would fire</span> · condition {op} {threshold}
        </span>
        <span>
          {r.execMs.toFixed(0)}ms{r.demo ? " · demo data" : ""}
        </span>
      </div>
      {r.error ? (
        <div className="px-3 py-2 font-mono text-[11.5px] text-[var(--color-danger)]">{r.error}</div>
      ) : r.results.length === 0 ? (
        <div className="px-3 py-3 font-mono text-[11px] text-[var(--color-fg-faint)]">The query returned no series — the rule would stay quiet.</div>
      ) : (
        <table className="w-full border-collapse font-mono text-[11.5px]">
          <tbody>
            {r.results.slice(0, 30).map((x, i) => (
              <tr key={i} className="border-b border-[var(--color-line)] last:border-b-0">
                <td className="px-3 py-1.5">
                  <span className="flex flex-wrap gap-1">
                    {Object.entries(x.labels).length === 0 ? (
                      <span className="text-[var(--color-fg-faint)]">{"{}"}</span>
                    ) : (
                      Object.entries(x.labels).map(([k, v]) => (
                        <span key={k} className="text-[var(--color-fg-dim)]">
                          <span className="text-[var(--color-syn-label)]">{k}</span>={v}
                        </span>
                      ))
                    )}
                  </span>
                </td>
                <td className="px-3 py-1.5 text-right tabular-nums text-[var(--color-fg)]">{x.value.toLocaleString("en-US", { maximumFractionDigits: 3 })}</td>
                <td className="px-3 py-1.5 text-right">{x.triggered ? <Chip tone="var(--color-accent)">fires</Chip> : <Chip tone="var(--color-signal)">ok</Chip>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function Field({ label, error, hint, group, children }: { label: string; error?: string; hint?: string; group?: boolean; children: React.ReactNode }) {
  // <label> gives the wrapped control its accessible name; a radio group
  // names itself, so it gets a plain container.
  const Wrap = group ? "div" : "label";
  return (
    <Wrap className="flex flex-col gap-1">
      <span className="flex items-baseline justify-between gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        <span>{label}</span>
        {hint && !error && <span className="truncate normal-case tracking-normal text-[var(--color-fg-faint)]">{hint}</span>}
        {error && (
          <span role="alert" className="normal-case tracking-normal text-[var(--color-danger)]">
            {error}
          </span>
        )}
      </span>
      {children}
    </Wrap>
  );
}
