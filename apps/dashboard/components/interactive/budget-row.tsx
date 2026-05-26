// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useState } from "react";
import { Loader2, ShieldCheck, ShieldOff } from "lucide-react";
import { useAppState } from "@/components/app-state";
import { useToast } from "@/components/toast";

export type BudgetRowData = {
  name: string;
  scope: string;
  ceilingUSD: number;
  spentPct: number;
  kind: "BudgetPolicy" | "CeilingPolicy";
};

function barColor(pct: number) {
  if (pct >= 90) return "var(--color-accent)";
  if (pct >= 70) return "var(--color-warn)";
  return "var(--color-signal)";
}

export function BudgetRow({ b }: { b: BudgetRowData }) {
  const { state, arm, disarm } = useAppState();
  const { toast } = useToast();
  const [inflight, setInflight] = useState(false);

  const armed = state.armed[b.name] ?? false;

  const toggle = async () => {
    setInflight(true);
    await new Promise((res) => setTimeout(res, 500));
    if (armed) {
      disarm(b.name);
      toast({
        tone: "info",
        title: `${b.name} disarmed`,
        sub: "policy now advisory · no escalation will run",
      });
    } else {
      arm(b.name);
      toast({
        tone: "ok",
        title: `${b.name} armed`,
        sub: "escalation plan ready · triggers will activate",
      });
    }
    setInflight(false);
  };

  return (
    <div className="bg-[var(--color-bg-raised)] px-4 py-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="flex items-center gap-3">
          <span className="font-mono text-[13px] text-[var(--color-fg)]">
            {b.name}
          </span>
          <span
            className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
            style={{
              color:
                b.kind === "CeilingPolicy"
                  ? "var(--color-cool)"
                  : "var(--color-fg-dim)",
            }}
          >
            {b.kind}
          </span>
        </div>
        <button
          type="button"
          onClick={toggle}
          disabled={inflight}
          className="inline-flex items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors disabled:opacity-60"
          style={{
            color: armed ? "var(--color-signal)" : "var(--color-fg-faint)",
            borderColor: armed ? "var(--color-signal)" : undefined,
          }}
        >
          {inflight ? (
            <Loader2 className="h-3 w-3 animate-spin" />
          ) : armed ? (
            <ShieldCheck className="h-3 w-3" />
          ) : (
            <ShieldOff className="h-3 w-3" />
          )}
          {armed ? "armed" : "arm policy"}
        </button>
      </div>
      <div className="mt-1 font-mono text-[11px] text-[var(--color-fg-dim)]">
        scope · {b.scope}
      </div>

      <div className="mt-3 flex items-center gap-3">
        <div className="relative h-[6px] flex-1 bg-[var(--color-line)]">
          <div
            className="absolute left-0 top-0 h-full"
            style={{ width: `${b.spentPct}%`, background: barColor(b.spentPct) }}
          />
        </div>
        <span
          className="w-14 shrink-0 text-right font-mono text-[11.5px] tabular-nums"
          style={{ color: barColor(b.spentPct) }}
        >
          {b.spentPct}%
        </span>
        <span className="w-28 shrink-0 text-right font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">
          of ${b.ceilingUSD.toLocaleString()}
        </span>
      </div>
    </div>
  );
}
