// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Small URL-param controls for explorer toolbars: a segmented switch, a
// labelled <select>, and a toggle. Each writes one search param with
// router.replace inside a transition (no scroll jump, pending state
// shown), so server components re-render with the new value.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useTransition, type ReactNode } from "react";

function useParamWriter() {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, start] = useTransition();
  const write = (patch: Record<string, string | null>) => {
    const next = new URLSearchParams(sp.toString());
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === "") next.delete(k);
      else next.set(k, v);
    }
    const qs = next.toString();
    start(() => router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false }));
  };
  return { sp, write, pending };
}

export function Segmented({
  param,
  options,
  value,
  defaultValue,
  label,
  clear = [],
}: {
  param: string;
  options: { value: string; label: ReactNode; title?: string }[];
  value: string;
  defaultValue?: string;
  label: string;
  /** Params to drop when this one changes (e.g. from/to when picking a window). */
  clear?: string[];
}) {
  const { write, pending } = useParamWriter();
  return (
    <div
      role="radiogroup"
      aria-label={label}
      // max-w-full + overflow-x-auto: on a phone a long option list scrolls
      // inside the control instead of widening the page.
      className={`inline-flex max-w-full items-center gap-0.5 overflow-x-auto rounded-[2px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-0.5 font-mono text-[10px] uppercase tracking-[0.12em] [scrollbar-width:none] ${pending ? "opacity-70" : ""}`}
    >
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            role="radio"
            aria-checked={on}
            title={o.title}
            onClick={() =>
              write({
                [param]: o.value === defaultValue ? null : o.value,
                ...Object.fromEntries(clear.map((k) => [k, null])),
              })
            }
            className="shrink-0 whitespace-nowrap rounded-[1px] px-2 py-1 transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
            style={{
              background: on ? "var(--color-fg)" : "transparent",
              color: on ? "var(--color-bg)" : "var(--color-fg-dim)",
            }}
          >
            {o.label}
          </button>
        );
      })}
    </div>
  );
}

export function ParamSelect({
  param,
  label,
  value,
  options,
  defaultValue = "",
  clear = [],
  allLabel,
}: {
  param: string;
  label: string;
  value: string;
  options: { value: string; label: string }[];
  defaultValue?: string;
  clear?: string[];
  /** When set, adds an empty "all" option with this label. */
  allLabel?: string;
}) {
  const { write, pending } = useParamWriter();
  return (
    <label
      className={`flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] focus-within:border-[var(--color-cool)] ${pending ? "opacity-70" : ""}`}
    >
      {label}
      <select
        value={value}
        onChange={(e) =>
          write({
            [param]: e.target.value === defaultValue ? null : e.target.value,
            ...Object.fromEntries(clear.map((k) => [k, null])),
          })
        }
        className="max-w-[220px] bg-transparent normal-case tracking-normal text-[var(--color-fg)] focus:outline-none"
      >
        {allLabel !== undefined && <option value="">{allLabel}</option>}
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}

export function ParamToggle({
  param,
  label,
  checked,
  title,
}: {
  param: string;
  label: string;
  checked: boolean;
  title?: string;
}) {
  const { write, pending } = useParamWriter();
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      title={title}
      onClick={() => write({ [param]: checked ? null : "1" })}
      className={`inline-flex items-center gap-2 border px-2 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] ${
        checked
          ? "border-[var(--color-cool)] text-[var(--color-cool)]"
          : "border-[var(--color-line-bright)] text-[var(--color-fg-faint)] hover:text-[var(--color-fg-dim)]"
      } bg-[var(--color-bg-sunken)] ${pending ? "opacity-70" : ""}`}
    >
      <span
        aria-hidden
        className="relative inline-block h-2.5 w-5 rounded-full border border-current"
      >
        <span
          className="absolute top-1/2 h-1.5 w-1.5 -translate-y-1/2 rounded-full bg-current transition-all"
          style={{ left: checked ? "calc(100% - 7px)" : "1px" }}
        />
      </span>
      {label}
    </button>
  );
}
