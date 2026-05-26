// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useEffect, useMemo, useState, useTransition } from "react";
import { useRouter, useSearchParams, usePathname } from "next/navigation";
import { Search, X } from "lucide-react";

// Generic search-and-facet toolbar for server-rendered tables.
//
// The component owns no data — it just edits the URL search params
// (`?q=foo&cloud=aws`). The server component above re-renders against
// the new params and filters its rows.
//
// Pass `facets` to render select-style chips alongside the freeform
// search box. Each facet writes its own search-param key.

export type Facet = {
  key: string;            // search-param key, e.g. "cloud"
  label: string;          // visible label, e.g. "cloud"
  options: { value: string; label: string }[];
};

export function TableFilter({
  placeholder = "search…",
  facets = [],
}: {
  placeholder?: string;
  facets?: Facet[];
}) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, startTransition] = useTransition();

  const initial = sp.get("q") ?? "";
  const [q, setQ] = useState(initial);

  // Sync local input with URL when navigation changes externally.
  useEffect(() => {
    setQ(sp.get("q") ?? "");
  }, [sp]);

  const facetValues = useMemo(() => {
    const m: Record<string, string> = {};
    for (const f of facets) m[f.key] = sp.get(f.key) ?? "";
    return m;
  }, [facets, sp]);

  const push = (next: URLSearchParams) => {
    const qs = next.toString();
    startTransition(() => {
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    });
  };

  const setParam = (key: string, value: string) => {
    const next = new URLSearchParams(sp.toString());
    if (value) next.set(key, value);
    else next.delete(key);
    push(next);
  };

  const onQueryChange = (v: string) => {
    setQ(v);
    setParam("q", v.trim());
  };

  const reset = () => {
    setQ("");
    push(new URLSearchParams());
  };

  const hasAny = q || facets.some((f) => facetValues[f.key]);

  return (
    <div className="mb-4 flex flex-wrap items-center gap-2">
      <label
        className={`flex min-w-[260px] flex-1 items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1.5 transition-colors ${
          pending ? "border-[var(--color-cool)]" : ""
        }`}
      >
        <Search className="h-3.5 w-3.5 text-[var(--color-fg-faint)]" />
        <input
          type="text"
          value={q}
          onChange={(e) => onQueryChange(e.target.value)}
          placeholder={placeholder}
          className="flex-1 bg-transparent font-mono text-[12px] text-[var(--color-fg)] placeholder:text-[var(--color-fg-faint)] focus:outline-none"
        />
        {q && (
          <button
            type="button"
            onClick={() => onQueryChange("")}
            className="text-[var(--color-fg-faint)] transition-colors hover:text-[var(--color-fg)]"
            aria-label="clear search"
          >
            <X className="h-3 w-3" />
          </button>
        )}
      </label>

      {facets.map((f) => (
        <FacetSelect
          key={f.key}
          facet={f}
          value={facetValues[f.key]}
          onChange={(v) => setParam(f.key, v)}
        />
      ))}

      {hasAny && (
        <button
          type="button"
          onClick={reset}
          className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:text-[var(--color-fg)]"
        >
          reset
        </button>
      )}
    </div>
  );
}

function FacetSelect({
  facet,
  value,
  onChange,
}: {
  facet: Facet;
  value: string;
  onChange: (v: string) => void;
}) {
  return (
    <label className="flex items-center gap-1.5 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
      {facet.label}
      <select
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="bg-transparent text-[var(--color-fg)] focus:outline-none"
      >
        <option value="">all</option>
        {facet.options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}
