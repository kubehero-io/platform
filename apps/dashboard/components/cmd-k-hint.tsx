// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { Search } from "lucide-react";
import { useSyncExternalStore } from "react";

/* Topbar button that opens the command palette (⌘K) for people who don't
   know the shortcut yet. */

const subscribe = () => () => {};
const isMac = () => /Mac|iPhone|iPad/.test(navigator.platform);

export function CmdKHint() {
  const mac = useSyncExternalStore(subscribe, isMac, () => true);
  return (
    <button
      type="button"
      onClick={() => window.dispatchEvent(new Event("kh:open-palette"))}
      className="hidden items-center gap-2 rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10.5px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] sm:flex"
      title="Open command palette"
      aria-label="Open command palette"
    >
      <Search className="h-3 w-3" aria-hidden />
      <span>search</span>
      <span className="ml-1 text-[var(--color-fg-faint)]">{mac ? "⌘K" : "Ctrl K"}</span>
    </button>
  );
}
