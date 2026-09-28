// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ChevronRight } from "lucide-react";
import { ClusterSwitcher } from "@/components/cluster-switcher";
import { CmdKHint } from "@/components/cmd-k-hint";
import { NotificationBell } from "@/components/notification-bell";
import { DEFAULT_RANGE, TimeRangeSelector, type RangeConfig } from "@/components/time-range-selector";
import { UserMenu } from "@/components/user-menu";
import { authMode } from "@/lib/auth-mode";
import { getSessionView } from "@/lib/session";

export async function Topbar({
  crumbs,
  range = DEFAULT_RANGE,
}: {
  crumbs: { label: string; href?: string }[];
  /** Time-range presets for this view; false hides the control. */
  range?: RangeConfig | false;
}) {
  const s = await getSessionView();
  const mode = authMode();
  const label = s?.email || (s?.subject ? s.subject : mode === "demo" ? "demo@kubehero.io" : "signed in");
  return (
    <header className="sticky top-0 z-20 flex h-12 items-center gap-3 border-b border-[var(--color-line)] bg-[var(--color-bg)]/90 px-5 backdrop-blur-md">
      <nav aria-label="Breadcrumb" className="flex min-w-0 items-center gap-2 font-mono text-[12px] text-[var(--color-fg-dim)]">
        {crumbs.map((c, i) => (
          <span key={i} className="flex min-w-0 items-center gap-2">
            {i > 0 && <ChevronRight className="h-3 w-3 shrink-0 text-[var(--color-fg-faint)]" aria-hidden />}
            {c.href ? (
              <Link href={c.href} className="truncate transition-colors hover:text-[var(--color-fg)]">
                {c.label}
              </Link>
            ) : (
              <span className="truncate text-[var(--color-fg)]" aria-current={i === crumbs.length - 1 ? "page" : undefined}>
                {c.label}
              </span>
            )}
          </span>
        ))}
      </nav>
      <ClusterSwitcher />
      <div className="ml-auto flex items-center gap-2">
        {range && <TimeRangeSelector config={range} />}
        <CmdKHint />
        <NotificationBell />
        <UserMenu label={label} org={s?.org || "kubehero"} role={s?.role} mode={mode} />
      </div>
    </header>
  );
}
