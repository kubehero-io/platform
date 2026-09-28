// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { useState, useTransition } from "react";
import { Loader2 } from "lucide-react";
import { expireSilence } from "@/app/(dashboard)/alerts/actions";
import { useToast } from "@/components/toast";

export function ExpireSilence({ id, disabled }: { id: string; disabled?: boolean }) {
  const [confirm, setConfirm] = useState(false);
  const [pending, start] = useTransition();
  const { toast } = useToast();
  if (disabled) return <span className="font-mono text-[10.5px] text-[var(--color-fg-faint)]" title="admin role required">—</span>;
  if (!confirm) {
    return (
      <button type="button" onClick={() => setConfirm(true)} className="font-mono text-[10.5px] uppercase tracking-[0.12em] text-[var(--color-fg-dim)] hover:text-[var(--color-warn)]">
        expire
      </button>
    );
  }
  return (
    <span className="inline-flex items-center gap-2 font-mono text-[10.5px]">
      <button
        type="button"
        disabled={pending}
        onClick={() =>
          start(async () => {
            const r = await expireSilence(id);
            if (r.ok) toast({ tone: "info", title: "Silence expired", sub: r.note });
            else toast({ tone: "err", title: "Couldn't expire silence", sub: r.error.message });
            setConfirm(false);
          })
        }
        className="text-[var(--color-warn)] hover:underline"
      >
        {pending ? <Loader2 className="h-3 w-3 animate-spin" aria-hidden /> : "confirm"}
      </button>
      <button type="button" onClick={() => setConfirm(false)} className="text-[var(--color-fg-faint)] hover:underline">
        cancel
      </button>
    </span>
  );
}
