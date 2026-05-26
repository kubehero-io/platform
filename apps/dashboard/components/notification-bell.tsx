// SPDX-License-Identifier: BUSL-1.1
"use client";

import { Bell } from "lucide-react";
import { useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import { useAppState, type Event } from "@/components/app-state";

function kindColor(k: Event["kind"]) {
  switch (k) {
    case "applied":    return "var(--color-signal)";
    case "armed":      return "var(--color-signal)";
    case "disarmed":   return "var(--color-fg-faint)";
    case "reverted":   return "var(--color-warn)";
    case "recommended":return "var(--color-cool)";
  }
}

export function NotificationBell() {
  const { state } = useAppState();
  const [open, setOpen] = useState(false);
  const count = state.events.length;

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="relative inline-flex items-center justify-center rounded-sm p-1.5 text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]"
        aria-label="Notifications"
      >
        <Bell className="h-3.5 w-3.5" />
        {count > 0 && (
          <span
            className="absolute -right-0.5 -top-0.5 inline-flex h-3.5 min-w-3.5 items-center justify-center rounded-full bg-[var(--color-accent)] px-1 font-mono text-[9px] text-white"
            aria-label={`${count} unread`}
          >
            {count > 9 ? "9+" : count}
          </span>
        )}
      </button>
      {open && (
        <div
          className="fixed inset-0 z-20"
          onClick={() => setOpen(false)}
          aria-hidden
        />
      )}
      <AnimatePresence>
        {open && (
          <motion.div
            initial={{ opacity: 0, y: -4 }}
            animate={{ opacity: 1, y: 0 }}
            exit={{ opacity: 0, y: -4 }}
            transition={{ duration: 0.15 }}
            className="absolute right-0 top-8 z-30 w-[380px] max-h-[480px] overflow-auto border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] shadow-xl"
          >
            <div className="flex items-center justify-between border-b border-[var(--color-line)] px-3 py-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <span>/// recent activity · this session</span>
              <span>{count} events</span>
            </div>
            {count === 0 ? (
              <div className="px-3 py-6 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
                no activity yet.{" "}
                <br />
                try applying a rightsize or arming a policy.
              </div>
            ) : (
              <div className="flex flex-col">
                {state.events.map((e) => (
                  <div
                    key={e.id}
                    className="flex items-start gap-3 border-b border-[var(--color-line)] px-3 py-2.5 last:border-b-0 font-mono text-[11px]"
                  >
                    <span
                      className="mt-1 h-1.5 w-1.5 shrink-0"
                      style={{ background: kindColor(e.kind) }}
                      aria-hidden
                    />
                    <div className="flex-1 min-w-0">
                      <div className="flex items-baseline justify-between gap-2">
                        <span
                          className="uppercase tracking-[0.14em]"
                          style={{ color: kindColor(e.kind) }}
                        >
                          {e.kind}
                        </span>
                        <span className="shrink-0 text-[var(--color-fg-faint)]">
                          {e.at.slice(11)}
                        </span>
                      </div>
                      <div className="mt-0.5 truncate text-[var(--color-fg)]">
                        {e.workload || e.policy || "—"}
                      </div>
                      {e.savingsK !== undefined && (
                        <div className="mt-0.5 text-[var(--color-signal)]">
                          −${e.savingsK.toFixed(1)}k / mo
                        </div>
                      )}
                      <div className="mt-0.5 text-[10px] text-[var(--color-fg-faint)]">
                        {e.auditId}
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </motion.div>
        )}
      </AnimatePresence>
    </div>
  );
}
