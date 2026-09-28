// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Free-text controls on /allocation that don't fit a <select>: the
// label:<key> aggregate and the shared-namespaces list. Both validate
// locally (the values end up in a query) and write URL params.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useState, useTransition } from "react";
import { Tag, Users } from "lucide-react";

const LABEL_KEY_RE = /^[a-zA-Z0-9]([-a-zA-Z0-9_./]{0,120}[a-zA-Z0-9])?$/;
const NS_RE = /^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/;

function useWrite() {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, start] = useTransition();
  return {
    sp,
    pending,
    write(patch: Record<string, string | null>) {
      const next = new URLSearchParams(sp.toString());
      for (const [k, v] of Object.entries(patch)) {
        if (v === null || v === "") next.delete(k);
        else next.set(k, v);
      }
      const qs = next.toString();
      start(() => router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false }));
    },
  };
}

export function LabelAggregate({ current }: { current: string }) {
  const { write, pending } = useWrite();
  const [key, setKey] = useState(current.startsWith("label:") ? current.slice(6) : "");
  const [err, setErr] = useState(false);
  const apply = () => {
    const k = key.trim();
    if (!LABEL_KEY_RE.test(k)) {
      setErr(true);
      return;
    }
    setErr(false);
    write({ agg: `label:${k}`, sort: null, dir: null });
  };
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        apply();
      }}
      className={`flex items-center gap-1.5 border bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] focus-within:border-[var(--color-cool)] ${
        err ? "border-[var(--color-danger)]" : current.startsWith("label:") ? "border-[var(--color-fg-dim)]" : "border-[var(--color-line-bright)]"
      } ${pending ? "opacity-70" : ""}`}
    >
      <Tag className="h-3 w-3" aria-hidden />
      <label htmlFor="agg-label">label</label>
      <input
        id="agg-label"
        value={key}
        onChange={(e) => setKey(e.target.value)}
        placeholder="app.kubernetes.io/name"
        spellCheck={false}
        maxLength={122}
        aria-invalid={err}
        className="w-40 bg-transparent font-mono text-[11px] normal-case tracking-normal text-[var(--color-fg)] placeholder:text-[var(--color-fg-faint)] focus:outline-none"
      />
    </form>
  );
}

export function SharedNamespaces({ current }: { current: string[] }) {
  const { write, pending } = useWrite();
  const [text, setText] = useState(current.join(", "));
  const [err, setErr] = useState<string | null>(null);
  const apply = () => {
    const list = [...new Set(text.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean))];
    const bad = list.find((n) => !NS_RE.test(n));
    if (bad) {
      setErr(`"${bad}" is not a namespace name`);
      return;
    }
    if (list.length > 20) {
      setErr("at most 20 namespaces");
      return;
    }
    setErr(null);
    write({ shared: list.join(",") || null });
  };
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        apply();
      }}
      title={err ?? "Namespaces whose cost is redistributed to everyone else, weighted by cost"}
      className={`flex items-center gap-1.5 border bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] focus-within:border-[var(--color-cool)] ${
        err ? "border-[var(--color-danger)]" : "border-[var(--color-line-bright)]"
      } ${pending ? "opacity-70" : ""}`}
    >
      <Users className="h-3 w-3" aria-hidden />
      <label htmlFor="shared-ns">shared</label>
      <input
        id="shared-ns"
        value={text}
        onChange={(e) => setText(e.target.value)}
        onBlur={apply}
        placeholder="kube-system, monitoring"
        spellCheck={false}
        maxLength={600}
        aria-invalid={!!err}
        className="w-44 bg-transparent font-mono text-[11px] normal-case tracking-normal text-[var(--color-fg)] placeholder:text-[var(--color-fg-faint)] focus:outline-none"
      />
    </form>
  );
}
