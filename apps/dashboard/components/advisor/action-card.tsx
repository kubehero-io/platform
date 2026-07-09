// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

"use client";

import { useState } from "react";
import Link from "next/link";
import { Check, ChevronDown, Copy, FileCode2, ShieldCheck } from "lucide-react";
import type { AdvisorActionDTO } from "@/lib/api/types";
import {
  formatImpactUsd,
  kindLabel,
  riskTone,
  splitTarget,
} from "@/lib/advisor-format";

export function ActionCard({ action, rank }: { action: AdvisorActionDTO; rank: number }) {
  const [open, setOpen] = useState(false);
  const [copied, setCopied] = useState(false);
  const tone = riskTone(action.risk);
  const t = splitTarget(action.target);

  const copyYaml = async () => {
    try {
      await navigator.clipboard.writeText(action.crdYaml);
      setCopied(true);
      setTimeout(() => setCopied(false), 1800);
    } catch {
      // Clipboard can be blocked (permissions / http); fail quietly —
      // the YAML is selectable right below the button.
    }
  };

  return (
    <div className="bg-[var(--color-bg-raised)]">
      {/* summary row */}
      <div className="grid gap-3 px-4 py-3.5 sm:grid-cols-[2.5rem_minmax(0,1fr)_auto] sm:items-start">
        <span className="font-mono text-[10px] tabular-nums text-[var(--color-fg-faint)]">
          {String(rank).padStart(2, "0")}
        </span>

        <div className="flex min-w-0 flex-col gap-1.5">
          <div className="flex flex-wrap items-center gap-2">
            <span className="border border-[var(--color-line-bright)] px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-cool)]">
              {kindLabel(action.kind)}
            </span>
            <span
              className="border px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-[0.14em]"
              style={{ color: tone, borderColor: "var(--color-line-bright)" }}
            >
              risk · {action.risk}
            </span>
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              {action.status}
            </span>
          </div>
          <h3 className="text-[14.5px] font-medium tracking-tight text-[var(--color-fg)]">
            {action.title}
          </h3>
          <Link
            href={
              t.workload
                ? `/workloads/${encodeURIComponent(t.cluster)}/${encodeURIComponent(t.namespace)}/${encodeURIComponent(t.workload)}`
                : `/clusters/${encodeURIComponent(t.cluster)}`
            }
            className="truncate font-mono text-[11px] text-[var(--color-fg-dim)] hover:text-[var(--color-cool)]"
          >
            {action.target}
          </Link>
          <p className="text-[12.5px] leading-relaxed text-[var(--color-fg-dim)]">
            {action.rationale}
          </p>
        </div>

        <div className="flex flex-col items-start gap-2 sm:items-end">
          <span className="font-mono text-[18px] tabular-nums tracking-tight text-[var(--color-signal)]">
            {formatImpactUsd(action.impactMonthlyUsd)}
          </span>
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            aria-expanded={open}
            className="inline-flex items-center gap-1.5 border border-[var(--color-line-bright)] px-2.5 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:border-[var(--color-fg-faint)] hover:text-[var(--color-fg)]"
          >
            <FileCode2 className="h-3 w-3" />
            view guarded crd
            <ChevronDown
              className="h-3 w-3 transition-transform"
              style={{ transform: open ? "rotate(180deg)" : undefined }}
            />
          </button>
        </div>
      </div>

      {/* expandable CRD panel */}
      {open && (
        <div className="border-t border-[var(--color-line)] px-4 py-3.5 sm:pl-[3.6rem]">
          <div className="border border-[var(--color-line)] bg-[var(--color-bg-sunken)]">
            <div className="flex items-center justify-between border-b border-[var(--color-line)] px-3 py-2">
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                /// guarded crd · {action.id}.yaml
              </span>
              <button
                type="button"
                onClick={copyYaml}
                className="inline-flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:text-[var(--color-fg)]"
              >
                {copied ? (
                  <>
                    <Check className="h-3 w-3 text-[var(--color-signal)]" />
                    copied
                  </>
                ) : (
                  <>
                    <Copy className="h-3 w-3" />
                    copy yaml
                  </>
                )}
              </button>
            </div>
            <pre className="overflow-x-auto p-3 font-mono text-[11.5px] leading-relaxed text-[var(--color-fg-dim)]">
              {action.crdYaml}
            </pre>
          </div>

          <div className="mt-3 flex items-start gap-2 border border-[var(--color-line)] bg-[var(--color-bg)] px-3 py-2.5">
            <ShieldCheck className="mt-px h-3.5 w-3.5 shrink-0 text-[var(--color-signal)]" />
            <p className="text-[11.5px] leading-relaxed text-[var(--color-fg-dim)]">
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                how this applies ·{" "}
              </span>
              The advisor never applies anything itself. You{" "}
              <code className="font-mono text-[var(--color-cool)]">kubectl apply</code> this
              policy, the KubeHero operator picks it up in observation mode, and its guards
              (<code className="font-mono text-[var(--color-cool)]">humanArm</code>,{" "}
              <code className="font-mono text-[var(--color-cool)]">mode: recommend</code>) keep
              it from mutating anything until a human arms it from the dashboard or CLI. Every
              step lands in the audit log and is reversible.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}
