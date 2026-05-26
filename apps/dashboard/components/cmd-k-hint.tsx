// SPDX-License-Identifier: BUSL-1.1
"use client";

import { Search } from "lucide-react";
import { useEffect, useState } from "react";

/* Tiny button that lives in the topbar as a keyboard-shortcut hint.
   Clicking it dispatches the same keydown the palette listens for so
   people who don't know the shortcut can still discover it. */

export function CmdKHint() {
  const [isMac, setIsMac] = useState(true);
  useEffect(() => {
    if (typeof navigator !== "undefined") {
      setIsMac(/Mac|iPhone|iPad/.test(navigator.platform));
    }
  }, []);

  const open = () => {
    const ev = new KeyboardEvent("keydown", {
      key: "k",
      metaKey: isMac,
      ctrlKey: !isMac,
      bubbles: true,
    });
    window.dispatchEvent(ev);
  };

  return (
    <button
      type="button"
      onClick={open}
      className="hidden items-center gap-2 rounded-sm border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10.5px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)] sm:flex"
      title="Open command palette"
      aria-label="Open command palette"
    >
      <Search className="h-3 w-3" />
      <span>search</span>
      <span className="ml-1 text-[var(--color-fg-faint)]">
        {isMac ? "⌘K" : "Ctrl K"}
      </span>
    </button>
  );
}
