// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Silence an alert: matchers (prefilled from the alert — alertname plus
// its identifying labels), a duration and a mandatory reason. Silences
// suppress notifications only; the alert keeps evaluating and stays
// visible, marked "silenced".

import { useState, useTransition } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { BellOff, Loader2, Lock, Plus, X } from "lucide-react";
import { addSilence } from "@/app/(dashboard)/alerts/actions";
import { useToast } from "@/components/toast";

const DURATIONS = [
  { m: 60, label: "1h" },
  { m: 240, label: "4h" },
  { m: 720, label: "12h" },
  { m: 1440, label: "1d" },
  { m: 4320, label: "3d" },
  { m: 10080, label: "7d" },
];

const input =
  "border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1.5 font-mono text-[12px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] focus:border-[var(--color-cool)]";

export function SilenceForm({ initial, canAdmin }: { initial: Record<string, string>; canAdmin: boolean }) {
  const [rows, setRows] = useState<{ k: string; v: string }[]>(() => {
    const r = Object.entries(initial).map(([k, v]) => ({ k, v }));
    return r.length ? r : [{ k: "alertname", v: "" }];
  });
  const [minutes, setMinutes] = useState(240);
  const [comment, setComment] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [pending, start] = useTransition();
  const { toast } = useToast();
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();

  const submit = () => {
    setError(null);
    const matchers = Object.fromEntries(rows.filter((r) => r.k.trim() && r.v.trim()).map((r) => [r.k.trim(), r.v.trim()]));
    start(async () => {
      const r = await addSilence({ matchers, durationMinutes: minutes, comment });
      if (!r.ok) {
        setError(r.error.message);
        return;
      }
      toast({ tone: r.demo ? "info" : "ok", title: "Silence created", sub: r.note ?? `for ${DURATIONS.find((d) => d.m === minutes)?.label}` });
      const next = new URLSearchParams(sp.toString());
      next.delete("silence");
      next.set("tab", "silences");
      router.replace(`${pathname}?${next.toString()}`, { scroll: false });
    });
  };

  return (
    <form
      className="flex flex-col gap-4 px-5 py-4"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      {!canAdmin && (
        <div className="flex items-start gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-3 py-2 font-mono text-[11px] text-[var(--color-fg-dim)]">
          <Lock className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
          Creating silences needs the admin role.
        </div>
      )}
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">matchers · all must equal</legend>
        {rows.map((r, i) => (
          <div key={i} className="flex items-center gap-2">
            <input aria-label={`matcher ${i + 1} label`} className={`${input} w-36`} value={r.k} onChange={(e) => setRows((xs) => xs.map((x, j) => (j === i ? { ...x, k: e.target.value } : x)))} placeholder="label" />
            <span className="font-mono text-[12px] text-[var(--color-fg-faint)]">=</span>
            <input aria-label={`matcher ${i + 1} value`} className={`${input} min-w-0 flex-1`} value={r.v} onChange={(e) => setRows((xs) => xs.map((x, j) => (j === i ? { ...x, v: e.target.value } : x)))} placeholder="value" />
            <button type="button" onClick={() => setRows((xs) => xs.filter((_, j) => j !== i))} aria-label="remove matcher" className="text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]">
              <X className="h-3.5 w-3.5" aria-hidden />
            </button>
          </div>
        ))}
        <button type="button" onClick={() => setRows((xs) => [...xs, { k: "", v: "" }])} className="inline-flex w-fit items-center gap-1 font-mono text-[11px] text-[var(--color-cool)] hover:text-[var(--color-fg)]">
          <Plus className="h-3 w-3" aria-hidden /> add matcher
        </button>
      </fieldset>
      <div className="flex flex-col gap-1">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">duration</span>
        <div role="radiogroup" aria-label="silence duration" className="flex flex-wrap gap-1">
          {DURATIONS.map((d) => (
            <button
              key={d.m}
              type="button"
              role="radio"
              aria-checked={minutes === d.m}
              onClick={() => setMinutes(d.m)}
              className={`border px-2.5 py-1 font-mono text-[11px] ${minutes === d.m ? "border-[var(--color-fg)] bg-[var(--color-fg)] text-[var(--color-bg)]" : "border-[var(--color-line-bright)] text-[var(--color-fg-dim)]"}`}
            >
              {d.label}
            </button>
          ))}
        </div>
      </div>
      <label className="flex flex-col gap-1">
        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">reason · recorded with the silence</span>
        <textarea className={`${input} min-h-[64px] resize-y`} value={comment} onChange={(e) => setComment(e.target.value)} maxLength={500} placeholder="Stripe incident INC-2231 — known, being handled" />
      </label>
      {error && (
        <div role="alert" className="border border-[var(--color-danger)]/50 bg-[var(--color-bg-sunken)] px-3 py-2 font-mono text-[11px] text-[var(--color-danger)]">
          {error}
        </div>
      )}
      <div className="flex items-center gap-2 border-t border-[var(--color-line)] pt-4">
        <button type="submit" disabled={pending || !canAdmin} className="btn-primary !px-3 !py-1.5 !text-[12px] disabled:cursor-not-allowed disabled:opacity-50">
          {pending ? <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden /> : <BellOff className="h-3.5 w-3.5" aria-hidden />}
          silence
        </button>
        <span className="font-mono text-[10.5px] text-[var(--color-fg-faint)]">notifications stop; the alert keeps evaluating.</span>
      </div>
    </form>
  );
}
