// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Tabs as links (?tab=…): deep-linkable, work without JS, and the server
// renders only the active tab's data. Server component.

import Link from "next/link";
import { hrefWith } from "@/lib/url";

export type TabDef = { id: string; label: string; count?: number | string; tone?: string };

export function UrlTabs({
  pathname,
  params,
  tabs,
  active,
  param = "tab",
  label = "views",
  resetKeys = [],
}: {
  pathname: string;
  params: Record<string, string>;
  tabs: TabDef[];
  active: string;
  param?: string;
  label?: string;
  /** Params that don't survive a tab switch (e.g. an open drawer id). */
  resetKeys?: string[];
}) {
  const reset = Object.fromEntries(resetKeys.map((k) => [k, null]));
  return (
    <nav aria-label={label} className="flex items-stretch gap-px overflow-x-auto border-b border-[var(--color-line)]">
      {tabs.map((t) => {
        const on = t.id === active;
        return (
          <Link
            key={t.id}
            href={hrefWith(pathname, params, { ...reset, [param]: t.id })}
            aria-current={on ? "page" : undefined}
            scroll={false}
            className={`relative -mb-px inline-flex items-center gap-2 whitespace-nowrap border-b px-3.5 py-2.5 font-mono text-[11px] uppercase tracking-[0.12em] transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:-outline-offset-2 focus-visible:outline-[var(--color-cool)] ${
              on
                ? "border-[var(--color-fg)] text-[var(--color-fg)]"
                : "border-transparent text-[var(--color-fg-faint)] hover:text-[var(--color-fg-dim)]"
            }`}
          >
            {t.label}
            {t.count !== undefined && (
              <span className="tabular-nums" style={{ color: t.tone ?? "var(--color-fg-faint)" }}>
                {t.count}
              </span>
            )}
          </Link>
        );
      })}
    </nav>
  );
}
