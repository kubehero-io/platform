// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

import { useState } from "react";
import { LogOut, Settings, User } from "lucide-react";
import { useRouter } from "next/navigation";
import { roleTone, type Role } from "@/lib/roles";

export type UserMenuProps = {
  /** Email, or the WhoAmI subject for API keys. */
  label: string;
  org: string;
  role?: Role;
  mode: "demo" | "token";
};

export function UserMenu({ label, org, role, mode }: UserMenuProps) {
  const [open, setOpen] = useState(false);
  const router = useRouter();

  const logout = async () => {
    await fetch("/api/auth/logout", { method: "POST" });
    router.push("/login?reason=signed_out");
    router.refresh();
  };

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        aria-haspopup="menu"
        aria-expanded={open}
        className="flex items-center gap-2 rounded-[2px] px-1 py-0.5 font-mono text-[10.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)]"
      >
        {role && (
          <span style={{ color: roleTone(role) }} title={`role · ${role}`}>
            {role}
          </span>
        )}
        <span className="text-[var(--color-line-bright)]">·</span>
        <span className="hidden max-w-[180px] truncate normal-case tracking-normal text-[var(--color-fg-dim)] lg:inline">
          {label}
        </span>
        <User className="h-3 w-3" aria-hidden />
      </button>
      {open && <div className="fixed inset-0 z-20" onClick={() => setOpen(false)} aria-hidden />}
      {open && (
        <div
          role="menu"
          className="absolute right-0 top-8 z-30 w-64 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] py-1 shadow-xl"
        >
          <div className="border-b border-[var(--color-line)] px-3 py-2">
            <div className="truncate font-mono text-[11px] text-[var(--color-fg)]">{label}</div>
            <div className="mt-0.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              {mode === "demo" ? `demo org · ${org}` : `org · ${org}`}
            </div>
            {role && (
              <div className="mt-1 font-mono text-[10px] uppercase tracking-[0.14em]" style={{ color: roleTone(role) }}>
                role · {role}
                {mode === "demo" && <span className="text-[var(--color-fg-faint)]"> · nothing persists</span>}
              </div>
            )}
          </div>
          <button
            type="button"
            role="menuitem"
            onClick={() => router.push("/settings")}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-[12.5px] text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)] hover:text-[var(--color-fg)]"
          >
            <Settings className="h-3 w-3" aria-hidden />
            Settings
          </button>
          <button
            type="button"
            role="menuitem"
            onClick={logout}
            className="flex w-full items-center gap-2 px-3 py-2 text-left text-[12.5px] text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)] hover:text-[var(--color-fg)]"
          >
            <LogOut className="h-3 w-3" aria-hidden />
            Sign out
          </button>
        </div>
      )}
    </div>
  );
}
