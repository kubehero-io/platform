// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Sortable column header: toggles ?sort=&dir= in the URL (see
// lib/table-sort.ts). Renders a real <button> inside the <th> so it is
// keyboard reachable, and sets aria-sort on the header cell.

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { useTransition } from "react";
import { ArrowDown, ArrowUp, ArrowUpDown } from "lucide-react";

export function SortHeader({
  column,
  label,
  align = "left",
  active,
  dir,
  className = "",
  title,
}: {
  column: string;
  label: string;
  align?: "left" | "right";
  /** Current sort column/direction, as resolved on the server. */
  active: boolean;
  dir: "asc" | "desc";
  className?: string;
  title?: string;
}) {
  const router = useRouter();
  const pathname = usePathname();
  const sp = useSearchParams();
  const [pending, start] = useTransition();

  const onClick = () => {
    const next = new URLSearchParams(sp.toString());
    next.set("sort", column);
    next.set("dir", active && dir === "desc" ? "asc" : "desc");
    start(() => router.replace(`${pathname}?${next.toString()}`, { scroll: false }));
  };

  const Icon = !active ? ArrowUpDown : dir === "desc" ? ArrowDown : ArrowUp;
  return (
    <th
      scope="col"
      aria-sort={active ? (dir === "desc" ? "descending" : "ascending") : "none"}
      className={`sticky top-0 z-[1] bg-[var(--color-bg-raised)] py-2 font-normal ${align === "right" ? "text-right" : "text-left"} ${className}`}
    >
      <button
        type="button"
        onClick={onClick}
        title={title}
        className={`inline-flex items-center gap-1 font-mono text-[10px] uppercase tracking-[0.14em] transition-colors hover:text-[var(--color-fg)] focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] ${
          active ? "text-[var(--color-fg-dim)]" : "text-[var(--color-fg-faint)]"
        } ${pending ? "opacity-60" : ""} ${align === "right" ? "flex-row-reverse" : ""}`}
      >
        {label}
        <Icon className={`h-3 w-3 ${active ? "" : "opacity-40"}`} aria-hidden />
      </button>
    </th>
  );
}

/** Plain sticky header cell for non-sortable columns. */
export function Th({
  children,
  align = "left",
  className = "",
}: {
  children?: React.ReactNode;
  align?: "left" | "right";
  className?: string;
}) {
  return (
    <th
      scope="col"
      className={`sticky top-0 z-[1] bg-[var(--color-bg-raised)] py-2 font-mono text-[10px] font-normal uppercase tracking-[0.14em] text-[var(--color-fg-faint)] ${align === "right" ? "text-right" : "text-left"} ${className}`}
    >
      {children}
    </th>
  );
}
