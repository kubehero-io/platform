// SPDX-License-Identifier: BUSL-1.1
"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useLocalStorageJSON } from "@/lib/use-local-storage";
import { ChevronDown, Layers, Search, Star } from "lucide-react";
import { cloudColor, stateMeta } from "@/lib/fleet-data";
import { useNavStatus } from "@/components/nav-status";
import type { NavStatus } from "@/lib/api/nav";

type SwitcherCluster = NavStatus["clusters"][number];

/* Federation UX: a cluster scoped to "all" or one specific cluster,
   accessible from anywhere. Borrows the Lens "hotbar" pattern —
   recently-visited clusters surface at the top so the most common
   switch is one click away rather than scroll-and-search.

   The `recent` list lives in localStorage keyed by cluster id; up to
   5 entries, MRU. Cleared by `kh.recent-clusters = []` if you want
   to reset.
*/

const RECENT_KEY = "kh.recent-clusters";
const RECENT_MAX = 5;

const NO_RECENT: string[] = [];

function clusterIdFromPath(path: string): string | null {
  const m = path.match(/^\/clusters\/([^/]+)/);
  if (!m) return null;
  try {
    return decodeURIComponent(m[1]);
  } catch {
    return null;
  }
}

function validateRecent(v: unknown): string[] | null {
  return Array.isArray(v) ? v.filter((s): s is string => typeof s === "string" && s.length <= 253).slice(0, RECENT_MAX) : null;
}

export function ClusterSwitcher() {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [recent, writeRecent] = useLocalStorageJSON(RECENT_KEY, NO_RECENT, validateRecent);
  const path = usePathname() ?? "";
  // Live clusters from /api/nav (demo fixture while in demo mode).
  const status = useNavStatus();
  const CLUSTERS: SwitcherCluster[] = useMemo(() => status?.clusters ?? [], [status]);

  // Record every cluster drill-in (MRU). Writes only when the order
  // actually changes, so the store update settles after one pass.
  useEffect(() => {
    const id = clusterIdFromPath(path);
    if (!id || recent[0] === id) return;
    writeRecent([id, ...recent.filter((x) => x !== id)].slice(0, RECENT_MAX));
  }, [path, recent, writeRecent]);

  // Keyboard: `g c` opens the switcher. Lens uses ⌘K for the global
  // palette (we already have that elsewhere); this is the focused
  // cluster-only switcher modeled on Lens's `Ctrl+L`.
  useEffect(() => {
    let armed = false;
    let armedTimer: ReturnType<typeof setTimeout> | null = null;
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (target && (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable)) return;
      if (!armed && e.key === "g") {
        armed = true;
        armedTimer = setTimeout(() => { armed = false; }, 800);
        return;
      }
      if (armed && e.key === "c") {
        e.preventDefault();
        setOpen(true);
        armed = false;
        if (armedTimer) clearTimeout(armedTimer);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => {
      window.removeEventListener("keydown", onKey);
      if (armedTimer) clearTimeout(armedTimer);
    };
  }, []);

  const activeId = clusterIdFromPath(path);
  const active = activeId ? CLUSTERS.find((c) => c.id === activeId) : null;
  const label = active ? active.name : "Fleet · all clusters";

  // Fuzzy filter — substring on name / cloud / region. The cmd-palette
  // (⌘K) does the heavier lift; this is the in-place narrowing for
  // people who already knew which switcher they wanted.
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return CLUSTERS;
    return CLUSTERS.filter((c) =>
      c.name.toLowerCase().includes(q) ||
      c.cloud.toLowerCase().includes(q) ||
      c.region.toLowerCase().includes(q),
    );
  }, [query, CLUSTERS]);

  const recentClusters = useMemo(
    () =>
      recent
        .map((id) => CLUSTERS.find((c) => c.id === id))
        .filter((c): c is SwitcherCluster => Boolean(c))
        .filter((c) => active?.id !== c.id)
        .slice(0, RECENT_MAX),
    [recent, active, CLUSTERS],
  );

  const close = useCallback(() => {
    setOpen(false);
    setQuery("");
  }, []);

  return (
    <div className="relative">
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        className="flex shrink-0 items-center gap-2 border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] px-2.5 py-1 font-mono text-[11px] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
        title="Switch cluster — press g then c"
        aria-label={`Switch cluster (${label})`}
      >
        <Layers className="h-3 w-3" />
        {/* Icon-only on phones: the label would push the header past the viewport. */}
        <span className="hidden max-w-[220px] truncate sm:inline">{label}</span>
        <ChevronDown className="h-3 w-3" />
      </button>

      {open && (
        <div className="fixed inset-0 z-20" aria-hidden onClick={close} />
      )}
      {open && (
        <div className="fixed inset-x-3 top-14 z-30 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] shadow-xl sm:absolute sm:inset-x-auto sm:left-0 sm:top-8 sm:w-[400px]">
          <div className="flex items-center justify-between border-b border-[var(--color-line)] px-3 py-2">
            <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              /// federation · {CLUSTERS.length} clusters
            </span>
            <span className="font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
              <kbd className="rounded-sm border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-1 py-px text-[9px]">g</kbd>{" "}
              <kbd className="rounded-sm border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-1 py-px text-[9px]">c</kbd>
            </span>
          </div>

          <label className="flex items-center gap-2 border-b border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-3 py-2">
            <Search className="h-3 w-3 text-[var(--color-fg-faint)]" />
            <input
              autoFocus
              type="text"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="search cluster name, cloud, or region…"
              className="flex-1 bg-transparent font-mono text-[11.5px] text-[var(--color-fg)] placeholder:text-[var(--color-fg-faint)] focus:outline-none"
            />
          </label>

          <Link
            href="/fleet"
            onClick={close}
            className={`flex items-center gap-2 border-b border-[var(--color-line)] px-3 py-2 text-[12.5px] transition-colors ${
              !active
                ? "bg-[var(--color-bg-sunken)] text-[var(--color-fg)]"
                : "text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
            }`}
          >
            <Layers className="h-3 w-3 text-[var(--color-fg-faint)]" />
            <span>Fleet · all clusters</span>
            <span className="ml-auto font-mono text-[10px] text-[var(--color-fg-faint)]">
              {CLUSTERS.reduce((s, c) => s + c.nodes, 0)} nodes
            </span>
          </Link>

          {recentClusters.length > 0 && !query && (
            <>
              <div className="flex items-center gap-1.5 border-b border-[var(--color-line)] bg-[var(--color-bg-sunken)]/40 px-3 py-1 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                <Star className="h-2.5 w-2.5" /> recent
              </div>
              {recentClusters.map((c) => (
                <ClusterRow key={`r-${c.id}`} cluster={c} active={false} onPick={close} dim />
              ))}
            </>
          )}

          <div className="max-h-[320px] overflow-y-auto">
            {!query && recentClusters.length > 0 && (
              <div className="border-b border-[var(--color-line)] bg-[var(--color-bg-sunken)]/40 px-3 py-1 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                all
              </div>
            )}
            {filtered.length === 0 ? (
              <div className="px-3 py-4 text-center font-mono text-[11px] text-[var(--color-fg-faint)]">
                no clusters match — try a different query
              </div>
            ) : (
              filtered.map((c) => (
                <ClusterRow
                  key={c.id}
                  cluster={c}
                  active={active?.id === c.id}
                  onPick={close}
                />
              ))
            )}
          </div>

          <div className="flex items-center justify-between border-t border-[var(--color-line)] px-3 py-2 font-mono text-[9.5px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <span>kubehero cluster add</span>
            <span>{filtered.length} of {CLUSTERS.length}</span>
          </div>
        </div>
      )}
    </div>
  );
}

function ClusterRow({
  cluster,
  active,
  onPick,
  dim = false,
}: {
  cluster: SwitcherCluster;
  active: boolean;
  onPick: () => void;
  dim?: boolean;
}) {
  const s = stateMeta[cluster.state];
  return (
    <Link
      href={`/clusters/${cluster.id}`}
      onClick={onPick}
      className={`flex items-center gap-2 border-b border-[var(--color-line)] px-3 py-2 font-mono text-[11.5px] transition-colors last:border-b-0 ${
        active
          ? "bg-[var(--color-bg-sunken)] text-[var(--color-fg)]"
          : dim
            ? "text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)]/50"
            : "text-[var(--color-fg-dim)] hover:bg-[var(--color-bg-sunken)]/50"
      }`}
    >
      <span
        className="h-1.5 w-1.5 shrink-0"
        style={{ background: cloudColor[cluster.cloud] }}
        aria-hidden
      />
      <span className="min-w-0 flex-1 truncate">{cluster.name}</span>
      <span
        className="shrink-0 rounded-sm border border-[var(--color-line)] px-1 py-px text-[9px] uppercase tracking-[0.12em] text-[var(--color-fg-faint)]"
        title={cluster.region}
      >
        {cluster.cloud}
      </span>
      <span
        className="shrink-0 text-[9.5px] uppercase tracking-[0.14em]"
        style={{ color: s.color }}
      >
        {s.label}
      </span>
      <span className="w-10 shrink-0 text-right text-[10px] tabular-nums text-[var(--color-fg-faint)]">
        {cluster.nodes}
      </span>
    </Link>
  );
}
