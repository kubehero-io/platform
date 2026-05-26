// SPDX-License-Identifier: BUSL-1.1
"use client";

import { useState } from "react";
import { LogOut, User } from "lucide-react";
import { useRouter } from "next/navigation";

export function UserMenu({ email, org }: { email: string; org: string }) {
  const [open, setOpen] = useState(false);
  const router = useRouter();

  const logout = async () => {
    await fetch("/api/auth/logout", { method: "POST" });
    router.push("/login");
    router.refresh();
  };

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex items-center gap-2 font-mono text-[10.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] hover:text-[var(--color-fg)]"
      >
        <span>org · {org}</span>
        <span className="text-[var(--color-line-bright)]">·</span>
        <span className="text-[var(--color-fg-dim)]">{email}</span>
        <User className="h-3 w-3" />
      </button>
      {open && (
        <div
          className="fixed inset-0 z-20"
          onClick={() => setOpen(false)}
          aria-hidden
        />
      )}
      {open && (
        <div className="absolute right-0 top-8 z-30 w-60 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] py-1 shadow-xl">
          <div className="border-b border-[var(--color-line)] px-3 py-2">
            <div className="font-mono text-[11px] text-[var(--color-fg)] truncate">
              {email}
            </div>
            <div className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              demo org · {org}
            </div>
          </div>
          <button
            type="button"
            onClick={() => router.push("/settings")}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-[12.5px] text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)] hover:text-[var(--color-fg)]"
          >
            Settings
          </button>
          <button
            type="button"
            onClick={logout}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-[12.5px] text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)] hover:text-[var(--color-fg)]"
          >
            <LogOut className="h-3 w-3" />
            Sign out
          </button>
        </div>
      )}
    </div>
  );
}
