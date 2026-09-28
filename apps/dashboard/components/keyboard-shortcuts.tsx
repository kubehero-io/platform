// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Global keyboard layer (inactive while typing in a field):
//   /        focus the page's search / query input ([data-search-input])
//   g <key>  jump to a view (keys from lib/nav.tsx; g c = cluster switcher)
//   j / k    next / previous row in the page's list ([data-nav-item])
//   ?        this help
// Lists with their own keyboard model (log lines, flamegraph) handle
// keys themselves when focused.

import { useRouter } from "next/navigation";
import { useEffect, useState } from "react";
import { X } from "lucide-react";
import { G_SHORTCUTS, NAV_ITEMS } from "@/lib/nav";

function typing(t: EventTarget | null): boolean {
  const el = t as HTMLElement | null;
  if (!el) return false;
  return el.tagName === "INPUT" || el.tagName === "TEXTAREA" || el.tagName === "SELECT" || el.isContentEditable || el.getAttribute("role") === "listbox";
}

function visible(el: HTMLElement): boolean {
  return !!(el.offsetWidth || el.offsetHeight || el.getClientRects().length);
}

export function KeyboardShortcuts() {
  const router = useRouter();
  const [help, setHelp] = useState(false);

  useEffect(() => {
    let armedUntil = 0;
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey || e.defaultPrevented) return;
      if (typing(e.target)) return;
      const now = Date.now();
      if (armedUntil > now) {
        armedUntil = 0;
        const href = G_SHORTCUTS[e.key];
        if (href) {
          e.preventDefault();
          router.push(href);
        }
        return;
      }
      if (e.key === "g") {
        armedUntil = now + 900;
        return;
      }
      if (e.key === "/") {
        const target = [...document.querySelectorAll<HTMLElement>("main [data-search-input]")].find(visible);
        if (target) {
          e.preventDefault();
          target.focus();
          if (target instanceof HTMLInputElement || target instanceof HTMLTextAreaElement) {
            const end = target.value.length;
            target.setSelectionRange(end, end);
          }
        }
        return;
      }
      if (e.key === "?") {
        e.preventDefault();
        setHelp((h) => !h);
        return;
      }
      if (e.key === "Escape") {
        setHelp(false);
        return;
      }
      if (e.key === "j" || e.key === "k") {
        const items = [...document.querySelectorAll<HTMLElement>("main [data-nav-item]")].filter(visible);
        if (items.length === 0) return;
        e.preventDefault();
        const cur = items.findIndex((el) => el === document.activeElement || el.contains(document.activeElement));
        const next = cur === -1 ? 0 : Math.max(0, Math.min(items.length - 1, cur + (e.key === "j" ? 1 : -1)));
        const el = items[next];
        if (el.tabIndex < 0 && !(el instanceof HTMLAnchorElement) && !(el instanceof HTMLButtonElement)) el.tabIndex = -1;
        el.focus({ preventScroll: true });
        el.scrollIntoView({ block: "nearest" });
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [router]);

  if (!help) return null;
  const rows: [string, string][] = [
    ["⌘K / Ctrl K", "command palette — views, clusters, actions, questions"],
    ["/", "focus the page's search or query box"],
    ["j / k", "next / previous row"],
    ["⏎", "open the focused row"],
    ["g c", "cluster switcher"],
    ...NAV_ITEMS.filter((i) => i.key).map((i) => [`g ${i.key}`, i.label] as [string, string]),
    ["?", "toggle this help"],
  ];
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-[var(--color-bg-sunken)]/80 px-4 backdrop-blur-sm" onClick={() => setHelp(false)}>
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Keyboard shortcuts"
        className="w-full max-w-[520px] border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] shadow-2xl"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-center justify-between border-b border-[var(--color-line)] px-4 py-2.5">
          <span className="section-label">/// keyboard</span>
          <button type="button" onClick={() => setHelp(false)} aria-label="Close" className="text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]">
            <X className="h-4 w-4" aria-hidden />
          </button>
        </div>
        <table className="w-full border-collapse">
          <tbody>
            {rows.map(([k, v]) => (
              <tr key={k} className="border-b border-[var(--color-line)] last:border-b-0">
                <td className="w-36 px-4 py-1.5">
                  <kbd className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-px font-mono text-[11px] text-[var(--color-fg)]">{k}</kbd>
                </td>
                <td className="px-4 py-1.5 text-[12.5px] text-[var(--color-fg-dim)]">{v}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
