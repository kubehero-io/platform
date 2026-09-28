// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Time-range control for the topbar. Relative presets write ?window=
// (and drop any absolute range); the custom popover writes ?from=&to= in
// epoch ms (UTC inputs, so the URL means the same thing for everyone).
// Drag-to-zoom on charts writes the same from/to params, so the picker
// always reflects what the page is showing.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useEffect, useRef, useState, useTransition } from "react";
import { CalendarRange, RotateCw } from "lucide-react";
import { parseInstant } from "@/lib/time-range";

export type RangeConfig = {
  options: readonly string[];
  defaultWindow: string;
  custom?: boolean;
  /** Show a refresh button (re-runs the server render with the same URL). */
  refresh?: boolean;
};

export const DEFAULT_RANGE: RangeConfig = { options: ["24h", "7d", "30d"], defaultWindow: "30d" };

const pad = (n: number) => String(n).padStart(2, "0");
/** epoch ms → "YYYY-MM-DDTHH:MM" in UTC for <input type=datetime-local>. */
function toInput(ms: number): string {
  const d = new Date(ms);
  return `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}`;
}
function fromInput(v: string): number | null {
  if (!v) return null;
  const t = Date.parse(`${v}:00Z`);
  return Number.isFinite(t) ? t : null;
}

export function TimeRangeSelector({ config = DEFAULT_RANGE }: { config?: RangeConfig }) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, startTransition] = useTransition();
  const [open, setOpen] = useState(false);
  const popRef = useRef<HTMLDivElement>(null);

  const from = parseInstant(sp.get("from") ?? undefined);
  const to = parseInstant(sp.get("to") ?? undefined);
  const isCustom = config.custom && from !== null && to !== null && to > from;
  const w = sp.get("window");
  const current = isCustom ? "custom" : w && config.options.includes(w) ? w : config.defaultWindow;

  const push = (next: URLSearchParams) => {
    const qs = next.toString();
    startTransition(() => router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false }));
  };

  const setWindow = (win: string) => {
    const next = new URLSearchParams(sp.toString());
    next.delete("from");
    next.delete("to");
    if (win === config.defaultWindow) next.delete("window");
    else next.set("window", win);
    push(next);
  };

  // Popover inputs, seeded from the current range when opened.
  const [a, setA] = useState("");
  const [b, setB] = useState("");
  const [err, setErr] = useState("");
  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (popRef.current && !popRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => e.key === "Escape" && setOpen(false);
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const openCustom = () => {
    const end = isCustom && to ? to : Date.now();
    const start = isCustom && from ? from : end - 3_600_000;
    setA(toInput(start));
    setB(toInput(end));
    setErr("");
    setOpen((o) => !o);
  };

  const applyCustom = () => {
    const s = fromInput(a);
    const e = fromInput(b);
    if (s === null || e === null || e <= s) {
      setErr("end must be after start");
      return;
    }
    if (e - s > 90 * 86_400_000) {
      setErr("max range is 90 days");
      return;
    }
    const next = new URLSearchParams(sp.toString());
    next.delete("window");
    next.set("from", String(s));
    next.set("to", String(e));
    setOpen(false);
    push(next);
  };

  return (
    <div className="relative flex items-center gap-1">
      <div
        className={`inline-flex items-center gap-0.5 rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-0.5 font-mono text-[10px] uppercase tracking-[0.14em] ${pending ? "opacity-70" : ""}`}
      >
        <div role="radiogroup" aria-label="time range" className="inline-flex items-center gap-0.5">
          {config.options.map((opt) => {
            const active = current === opt;
            return (
              <button
                key={opt}
                type="button"
                role="radio"
                aria-checked={active}
                onClick={() => setWindow(opt)}
                disabled={pending}
                className="px-2 py-0.5 transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] disabled:opacity-60"
                style={{
                  background: active ? "var(--color-fg)" : "transparent",
                  color: active ? "var(--color-bg)" : "var(--color-fg-dim)",
                }}
              >
                {opt}
              </button>
            );
          })}
        </div>
        {/* Opens a dialog, so a plain button rather than a radio. */}
        {config.custom && (
          <button
            type="button"
            aria-label={current === "custom" ? "Custom range (active)" : "Custom range"}
            aria-haspopup="dialog"
            aria-expanded={open}
            onClick={openCustom}
            title="Custom absolute range (UTC)"
            className="inline-flex items-center gap-1 px-2 py-0.5 transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
            style={{
              background: current === "custom" ? "var(--color-fg)" : "transparent",
              color: current === "custom" ? "var(--color-bg)" : "var(--color-fg-dim)",
            }}
          >
            <CalendarRange className="h-3 w-3" aria-hidden />
            {current === "custom" ? "custom" : ""}
          </button>
        )}
      </div>
      {config.refresh && (
        <button
          type="button"
          onClick={() => startTransition(() => router.refresh())}
          aria-label="Refresh"
          title="Re-run the queries for this range"
          className="rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-1 text-[var(--color-fg-faint)] transition-colors hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
        >
          <RotateCw className={`h-3 w-3 ${pending ? "animate-spin" : ""}`} aria-hidden />
        </button>
      )}
      {open && (
        <div
          ref={popRef}
          role="dialog"
          aria-label="Custom time range"
          className="absolute right-0 top-8 z-30 w-[300px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-3 shadow-xl"
        >
          <div className="mb-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">/// absolute range · utc</div>
          <label className="mb-2 flex flex-col gap-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            from
            <input
              type="datetime-local"
              value={a}
              onChange={(e) => setA(e.target.value)}
              className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[12px] normal-case tracking-normal text-[var(--color-fg)] [color-scheme:dark] focus:border-[var(--color-cool)] focus:outline-none"
            />
          </label>
          <label className="mb-3 flex flex-col gap-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            to
            <input
              type="datetime-local"
              value={b}
              onChange={(e) => setB(e.target.value)}
              className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[12px] normal-case tracking-normal text-[var(--color-fg)] [color-scheme:dark] focus:border-[var(--color-cool)] focus:outline-none"
            />
          </label>
          {err && <div className="mb-2 font-mono text-[11px] text-[var(--color-danger)]">{err}</div>}
          <div className="flex justify-end gap-2">
            <button type="button" onClick={() => setOpen(false)} className="btn-secondary !px-2.5 !py-1 !text-[12px]">
              cancel
            </button>
            <button type="button" onClick={applyCustom} className="btn-primary !px-2.5 !py-1 !text-[12px]">
              apply
            </button>
          </div>
        </div>
      )}
    </div>
  );
}
