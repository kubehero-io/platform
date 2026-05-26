// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useState } from "react";
import { CheckCircle2, Loader2, XCircle } from "lucide-react";
import { useToast } from "@/components/toast";

export type Integration = {
  id: string;
  name: string;
  kind: string;
  note: string;
};

export function IntegrationRow({ integration }: { integration: Integration }) {
  const [state, setState] = useState<"off" | "connecting" | "on">("off");
  const { toast } = useToast();

  const toggle = async () => {
    if (state === "on") {
      setState("off");
      toast({ tone: "info", title: `${integration.name} disconnected` });
      return;
    }
    setState("connecting");
    // Simulate OAuth roundtrip
    await new Promise((res) => setTimeout(res, 1200));
    setState("on");
    toast({
      tone: "ok",
      title: `${integration.name} connected`,
      sub: "events will route through this channel",
    });
  };

  return (
    <div className="flex items-center gap-3 bg-[var(--color-bg-raised)] px-4 py-3 font-mono">
      <span
        className="h-1.5 w-1.5 shrink-0"
        style={{
          background:
            state === "on"
              ? "var(--color-signal)"
              : state === "connecting"
                ? "var(--color-cool)"
                : "var(--color-fg-faint)",
        }}
        aria-hidden
      />
      <span className="min-w-0 flex-1">
        <span className="flex items-baseline justify-between gap-2">
          <span className="text-[12.5px] text-[var(--color-fg)]">
            {integration.name}
          </span>
          <span className="shrink-0 text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            {integration.kind}
          </span>
        </span>
        <span className="mt-0.5 block text-[11px] text-[var(--color-fg-dim)]">
          {integration.note}
        </span>
      </span>
      <button
        type="button"
        onClick={toggle}
        disabled={state === "connecting"}
        className="shrink-0 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 text-[10px] uppercase tracking-[0.14em] transition-colors disabled:opacity-60"
        style={{
          color:
            state === "on"
              ? "var(--color-signal)"
              : state === "connecting"
                ? "var(--color-cool)"
                : "var(--color-cool)",
          borderColor: state === "on" ? "var(--color-signal)" : undefined,
        }}
      >
        {state === "connecting" ? (
          <span className="inline-flex items-center gap-1.5">
            <Loader2 className="h-3 w-3 animate-spin" /> connecting
          </span>
        ) : state === "on" ? (
          <span className="inline-flex items-center gap-1.5">
            <CheckCircle2 className="h-3 w-3" /> connected
          </span>
        ) : (
          <span className="inline-flex items-center gap-1.5">
            <XCircle className="h-3 w-3" /> connect
          </span>
        )}
      </button>
    </div>
  );
}
