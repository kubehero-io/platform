// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { ChevronRight } from "lucide-react";
import { ClusterSwitcher } from "@/components/cluster-switcher";
import { CmdKHint } from "@/components/cmd-k-hint";
import { NotificationBell } from "@/components/notification-bell";
import { TimeRangeSelector } from "@/components/time-range-selector";
import { UserMenu } from "@/components/user-menu";
import { authMode } from "@/lib/auth-mode";
import { getSessionView } from "@/lib/session";

export async function Topbar({
  crumbs,
}: {
  crumbs: { label: string; href?: string }[];
}) {
  const s = await getSessionView();
  const mode = authMode();
  const label = s?.email || (s?.subject ? s.subject : mode === "demo" ? "demo@kubehero.io" : "signed in");
  return (
    <header className="sticky top-0 z-10 flex h-12 items-center gap-3 border-b border-[var(--color-line)] bg-[var(--color-bg)]/90 px-5 backdrop-blur-md">
      <nav className="flex items-center gap-2 font-mono text-[12px] text-[var(--color-fg-dim)]">
        {crumbs.map((c, i) => (
          <span key={i} className="flex items-center gap-2">
            {i > 0 && <ChevronRight className="h-3 w-3 text-[var(--color-fg-faint)]" />}
            {c.href ? (
              <a href={c.href} className="transition-colors hover:text-[var(--color-fg)]">
                {c.label}
              </a>
            ) : (
              <span className="text-[var(--color-fg)]">{c.label}</span>
            )}
          </span>
        ))}
      </nav>
      <ClusterSwitcher />
      <div className="ml-auto flex items-center gap-2">
        <TimeRangeSelector />
        <CmdKHint />
        <NotificationBell />
        <UserMenu label={label} org={s?.org || "kubehero"} role={s?.role} mode={mode} />
      </div>
    </header>
  );
}
