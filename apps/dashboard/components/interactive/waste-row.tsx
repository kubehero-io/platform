// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useState } from "react";
import Link from "next/link";
import { CheckCircle2, Loader2, RotateCcw } from "lucide-react";
import { AnimatePresence, motion } from "motion/react";
import { useAppState } from "@/components/app-state";
import { useToast } from "@/components/toast";
import { cloudColor, type Cloud } from "@/lib/fleet-data";

export type WasteRowData = {
  rank: string;
  workload: string;
  ns: string;
  cluster: string;
  cloud: Cloud;
  detail: string;
  amountK: number;
  tone: string;
  action: "apply" | "review" | null;
};

export function WasteRow({ r }: { r: WasteRowData }) {
  const { state, apply, revert } = useAppState();
  const { toast } = useToast();
  const [inflight, setInflight] = useState(false);

  const applied = !!state.applied[r.workload];

  const onApply = async () => {
    setInflight(true);
    // Simulate rightsize execution: kubectl call + verify + log
    await new Promise((res) => setTimeout(res, 1200));
    apply(r.workload, r.amountK, r.cluster);
    toast({
      tone: "ok",
      title: `${r.workload} right-sized`,
      sub: `saved $${r.amountK.toFixed(1)}k / mo · reversible 10m`,
    });
    setInflight(false);
  };

  const onRevert = () => {
    revert(r.workload);
    toast({
      tone: "info",
      title: `${r.workload} reverted`,
      sub: `original request restored · audit-logged`,
    });
  };

  return (
    <tr className="border-b border-[var(--color-line)] last:border-b-0">
      <td className="py-3 pl-4 font-mono text-[10.5px] tabular-nums text-[var(--color-fg-faint)]">
        {r.rank}
      </td>
      <td className="py-3 pr-3 font-mono text-[12.5px] text-[var(--color-fg)]">
        <Link
          href={`/workloads/${encodeURIComponent(r.cluster)}/${encodeURIComponent(r.ns)}/${encodeURIComponent(r.workload)}`}
          className="hover:text-[var(--color-cool)]"
          style={{
            textDecoration: applied ? "line-through" : undefined,
            color: applied ? "var(--color-fg-dim)" : undefined,
          }}
        >
          {r.workload}
        </Link>
      </td>
      <td className="py-3 pr-3 font-mono text-[12px] text-[var(--color-fg-dim)]">
        {r.ns}
      </td>
      <td className="py-3 pr-3">
        <Link
          href={`/clusters/${r.cluster}`}
          className="inline-flex items-center gap-1.5 font-mono text-[12px] text-[var(--color-fg-dim)] hover:text-[var(--color-cool)]"
        >
          <span
            className="h-1.5 w-1.5"
            style={{ background: cloudColor[r.cloud] }}
            aria-hidden
          />
          {r.cluster}
        </Link>
      </td>
      <td className="py-3 pr-3 font-mono text-[11.5px] text-[var(--color-fg-dim)]">
        {r.detail}
      </td>
      <td
        className="py-3 pr-3 text-right font-mono tabular-nums"
        style={{ color: applied ? "var(--color-signal)" : r.tone }}
      >
        <AnimatePresence mode="wait">
          {applied ? (
            <motion.span key="saved" initial={{ opacity: 0 }} animate={{ opacity: 1 }}>
              −${r.amountK.toFixed(1)}k saved
            </motion.span>
          ) : (
            <motion.span key="wasting" initial={{ opacity: 0 }} animate={{ opacity: 1 }}>
              ${r.amountK.toFixed(1)}k / mo
            </motion.span>
          )}
        </AnimatePresence>
      </td>
      <td className="py-3 pr-4 text-right">
        {applied ? (
          <button
            type="button"
            onClick={onRevert}
            className="inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
          >
            <RotateCcw className="h-3 w-3" />
            revert
          </button>
        ) : r.action ? (
          <button
            type="button"
            onClick={onApply}
            disabled={inflight}
            className="inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors hover:border-[var(--color-cool)] hover:text-[var(--color-cool)] disabled:opacity-60"
            style={{
              color: r.action === "apply" ? "var(--color-cool)" : "var(--color-warn)",
            }}
          >
            {inflight ? (
              <>
                <Loader2 className="h-3 w-3 animate-spin" /> applying
              </>
            ) : (
              <>→ {r.action}</>
            )}
          </button>
        ) : null}
      </td>
    </tr>
  );
}

export function AppliedBannerStat() {
  const { state } = useAppState();
  const savedK = Object.entries(state.applied)
    .filter(([, v]) => v)
    .reduce((sum, [w]) => {
      // We don't carry savings in state; compute from events instead
      const ev = state.events.find((e) => e.workload === w && e.kind === "applied");
      return sum + (ev?.savingsK ?? 0);
    }, 0);

  if (savedK === 0) return null;
  return (
    <div className="mb-4 flex items-center gap-3 border border-[var(--color-signal)]/40 bg-[var(--color-bg-raised)] px-4 py-2.5 font-mono text-[12px]">
      <CheckCircle2 className="h-3.5 w-3.5 text-[var(--color-signal)]" />
      <span className="text-[var(--color-fg)]">
        −${savedK.toFixed(1)}k / mo saved this session
      </span>
      <span className="ml-auto text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        reversible · see ceiling log
      </span>
    </div>
  );
}
