// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useTransition } from "react";
import { useRouter, usePathname, useSearchParams } from "next/navigation";

// Segmented 24h / 7d / 30d control. Reads + writes ?window= in the URL
// so the server component above re-fetches with the new window value.
//
// Lives in the topbar so it's available on every dashboard view; pages
// that don't care about window simply ignore the search param.

const WINDOWS = ["24h", "7d", "30d"] as const;
type Window = (typeof WINDOWS)[number];

const DEFAULT_WINDOW: Window = "30d";

export function TimeRangeSelector() {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, startTransition] = useTransition();

  const current = (sp.get("window") as Window) ?? DEFAULT_WINDOW;

  const set = (w: Window) => {
    const next = new URLSearchParams(sp.toString());
    if (w === DEFAULT_WINDOW) next.delete("window");
    else next.set("window", w);
    const qs = next.toString();
    startTransition(() => {
      router.replace(qs ? `${pathname}?${qs}` : pathname, { scroll: false });
    });
  };

  return (
    <div
      className="inline-flex items-center gap-0.5 rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
      aria-label="time range"
    >
      {WINDOWS.map((w) => {
        const active = current === w;
        return (
          <button
            key={w}
            type="button"
            onClick={() => set(w)}
            disabled={pending}
            className="px-2 py-0.5 transition-colors disabled:opacity-60"
            style={{
              background: active ? "var(--color-fg)" : "transparent",
              color: active ? "var(--color-bg)" : "var(--color-fg-dim)",
            }}
          >
            {w}
          </button>
        );
      })}
    </div>
  );
}
