// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// The "apply via policy" CTA: mode switch + the generated RightsizingPolicy
// YAML + copy. Regenerated client-side from the same pure generator the
// server uses, so switching modes is instant.

import { useState } from "react";
import { ShieldCheck } from "lucide-react";
import { CopyButton } from "@/components/ui/copy-button";
import { policyName, rightsizingPolicyYaml, type PolicyInput, type PolicyMode } from "@/lib/rightsizing/policy";

const MODES: { id: PolicyMode; label: string; hint: string }[] = [
  { id: "recommend", label: "recommend", hint: "status only" },
  { id: "shadow", label: "shadow", hint: "audit what would change" },
  { id: "apply", label: "apply", hint: "patch after arming" },
];

export function PolicyPanel({ input }: { input: PolicyInput }) {
  const [mode, setMode] = useState<PolicyMode>("apply");
  const yaml = rightsizingPolicyYaml(input, mode);
  return (
    <div className="border border-[var(--color-line)] bg-[var(--color-bg-sunken)]">
      <div className="flex flex-wrap items-center justify-between gap-2 border-b border-[var(--color-line)] px-3 py-2">
        <div role="radiogroup" aria-label="policy mode" className="inline-flex gap-0.5 font-mono text-[10px] uppercase tracking-[0.12em]">
          {MODES.map((m) => (
            <button
              key={m.id}
              type="button"
              role="radio"
              aria-checked={mode === m.id}
              title={m.hint}
              onClick={() => setMode(m.id)}
              className="px-2 py-1 transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
              style={{
                background: mode === m.id ? "var(--color-fg)" : "transparent",
                color: mode === m.id ? "var(--color-bg)" : "var(--color-fg-dim)",
              }}
            >
              {m.label}
            </button>
          ))}
        </div>
        <CopyButton text={yaml} label={`copy ${policyName(input.namespace, input.workload)}.yaml`} />
      </div>
      <pre className="max-h-[360px] overflow-auto p-3 font-mono text-[11px] leading-relaxed text-[var(--color-fg-dim)]">{yaml}</pre>
      <div className="flex items-start gap-2 border-t border-[var(--color-line)] px-3 py-2.5">
        <ShieldCheck className="mt-0.5 h-3.5 w-3.5 shrink-0 text-[var(--color-signal)]" aria-hidden />
        <p className="text-[11.5px] leading-relaxed text-[var(--color-fg-dim)]">
          The dashboard never changes a workload. The operator applies this policy only after a human arms it, and
          re-checks every guard first — confidence, OOM history, rollout state, at most one change a day. Every step
          is audited and reversible with <code className="font-mono text-[var(--color-cool)]">kubehero undo</code>.
        </p>
      </div>
    </div>
  );
}
