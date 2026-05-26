// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useState } from "react";
import { Loader2, Play } from "lucide-react";
import { useToast } from "@/components/toast";

export type Exporter = {
  id: string;
  name: string;
  cli: string;
  example: string;
  tint: string;
};

export function AuditExportRow({ ex }: { ex: Exporter }) {
  const [active, setActive] = useState(false);
  const [testing, setTesting] = useState(false);
  const { toast } = useToast();

  const test = async () => {
    setTesting(true);
    await new Promise((r) => setTimeout(r, 1200));
    setTesting(false);
    toast({
      tone: "ok",
      title: `${ex.name} test message sent`,
      sub: `delivered: 1 event · auth: HMAC-signed`,
    });
  };

  return (
    <div className="flex flex-col gap-2 bg-[var(--color-bg-raised)] px-4 py-3">
      <div className="flex items-center gap-3">
        <span
          className="h-1.5 w-1.5 shrink-0"
          style={{ background: active ? ex.tint : "var(--color-fg-faint)" }}
          aria-hidden
        />
        <span className="flex-1 font-mono text-[12.5px] text-[var(--color-fg)]">
          {ex.name}
        </span>
        <label className="inline-flex cursor-pointer items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)]">
          <input
            type="checkbox"
            checked={active}
            onChange={(e) => setActive(e.target.checked)}
            className="sr-only"
          />
          <span
            className="relative inline-block h-3.5 w-7 rounded-sm border"
            style={{
              borderColor: active ? ex.tint : "var(--color-line-bright)",
              background: active ? ex.tint : "var(--color-bg-sunken)",
            }}
          >
            <span
              className="absolute top-0 h-full w-3 bg-[var(--color-bg-raised)] transition-all"
              style={{ left: active ? "14px" : "0px" }}
            />
          </span>
          {active ? "enabled" : "off"}
        </label>
        <button
          type="button"
          onClick={test}
          disabled={!active || testing}
          className="shrink-0 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2 py-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-cool)] disabled:opacity-40"
        >
          {testing ? (
            <span className="inline-flex items-center gap-1.5">
              <Loader2 className="h-3 w-3 animate-spin" /> testing
            </span>
          ) : (
            <span className="inline-flex items-center gap-1.5">
              <Play className="h-3 w-3" /> send test
            </span>
          )}
        </button>
      </div>
      <code className="block rounded-sm border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-2 py-1.5 font-mono text-[10.5px] text-[var(--color-fg-dim)]">
        {ex.cli}{" "}
        <span className="text-[var(--color-cool)]">{ex.example}</span>
      </code>
    </div>
  );
}
