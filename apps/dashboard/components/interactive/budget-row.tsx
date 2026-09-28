// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { useState, useTransition } from "react";
import { Loader2, Lock, ShieldCheck, ShieldOff } from "lucide-react";
import { useAppState } from "@/components/app-state";
import { useToast } from "@/components/toast";
import { setPolicyArmed } from "@/app/(dashboard)/budgets/actions";

export type BudgetRowData = {
  name: string;
  scope: string;
  ceilingUSD: number;
  spentPct: number;
  kind: "BudgetPolicy" | "CeilingPolicy";
  /** Live rows carry the server's armed bit; demo rows use local state. */
  armed?: boolean;
};

function barColor(pct: number) {
  if (pct >= 90) return "var(--color-accent)";
  if (pct >= 70) return "var(--color-warn)";
  return "var(--color-signal)";
}

export function BudgetRow({
  b,
  live = false,
  canArm = true,
}: {
  b: BudgetRowData;
  /** Arming goes through the ArmPolicy RPC (true) or local demo state (false). */
  live?: boolean;
  /** Admin role — hides the toggle's effect for viewers/members. */
  canArm?: boolean;
}) {
  const { state, arm, disarm } = useAppState();
  const { toast } = useToast();
  const [pending, startTransition] = useTransition();
  // Optimistic override for live rows until the refreshed list arrives.
  const [optimistic, setOptimistic] = useState<boolean | null>(null);

  const armed = live ? (optimistic ?? b.armed ?? false) : (state.armed[b.name] ?? false);

  const toggle = () => {
    const nextArmed = !armed;
    if (!live) {
      if (nextArmed) arm(b.name);
      else disarm(b.name);
      toast({
        tone: nextArmed ? "ok" : "info",
        title: `${b.name} ${nextArmed ? "armed" : "disarmed"}`,
        sub: "demo mode · nothing persisted to a control plane",
      });
      return;
    }
    setOptimistic(nextArmed);
    startTransition(async () => {
      const res = await setPolicyArmed({
        policyName: b.name,
        armed: nextArmed,
        reason: `${nextArmed ? "armed" : "disarmed"} from the dashboard`,
      });
      if (!res.ok) {
        setOptimistic(null);
        toast({ tone: "err", title: `Couldn't ${nextArmed ? "arm" : "disarm"} ${b.name}`, sub: res.error.message });
        return;
      }
      toast({
        tone: nextArmed ? "ok" : "info",
        title: `${b.name} ${nextArmed ? "armed" : "disarmed"}`,
        sub: res.data?.auditId ? `audit ${res.data.auditId} · reversible` : (res.note ?? "recorded in the audit log"),
      });
    });
  };

  const disabled = pending || !canArm;

  return (
    <div className="bg-[var(--color-bg-raised)] px-4 py-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div className="flex items-center gap-3">
          <span className="font-mono text-[13px] text-[var(--color-fg)]">{b.name}</span>
          <span
            className="border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
            style={{ color: b.kind === "CeilingPolicy" ? "var(--color-cool)" : "var(--color-fg-dim)" }}
          >
            {b.kind}
          </span>
        </div>
        <button
          type="button"
          onClick={toggle}
          disabled={disabled}
          aria-pressed={armed}
          title={!canArm ? "Arming a policy requires the admin role" : undefined}
          className="inline-flex items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] disabled:cursor-not-allowed disabled:opacity-60"
          style={{
            color: armed ? "var(--color-signal)" : "var(--color-fg-faint)",
            borderColor: armed ? "var(--color-signal)" : undefined,
          }}
        >
          {pending ? (
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
          ) : !canArm ? (
            <Lock className="h-3 w-3" aria-hidden />
          ) : armed ? (
            <ShieldCheck className="h-3 w-3" aria-hidden />
          ) : (
            <ShieldOff className="h-3 w-3" aria-hidden />
          )}
          {armed ? "armed" : canArm ? "arm policy" : "disarmed · admin only"}
        </button>
      </div>
      <div className="mt-1 font-mono text-[11px] text-[var(--color-fg-dim)]">scope · {b.scope}</div>

      <div className="mt-3 flex items-center gap-3">
        <div
          className="relative h-[6px] flex-1 bg-[var(--color-line)]"
          role="meter"
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={b.spentPct}
          aria-label={`${b.name} spent`}
        >
          <div
            className="absolute left-0 top-0 h-full"
            style={{ width: `${Math.min(100, Math.max(0, b.spentPct))}%`, background: barColor(b.spentPct) }}
          />
        </div>
        <span className="w-14 shrink-0 text-right font-mono text-[11.5px] tabular-nums" style={{ color: barColor(b.spentPct) }}>
          {b.spentPct}%
        </span>
        <span className="w-28 shrink-0 text-right font-mono text-[11.5px] tabular-nums text-[var(--color-fg-dim)]">
          of ${b.ceilingUSD.toLocaleString("en-US")}
        </span>
      </div>
    </div>
  );
}
