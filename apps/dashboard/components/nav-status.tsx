// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Shell-wide status (badges, footer, cluster list) fetched from /api/nav
// on mount, every minute while the tab is visible, and on focus. One
// fetch feeds the sidebar, the cluster switcher and ⌘K.

import { createContext, useCallback, useContext, useEffect, useState } from "react";
import type { NavStatus } from "@/lib/api/nav";

const Ctx = createContext<NavStatus | null>(null);

export function NavStatusProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<NavStatus | null>(null);

  const load = useCallback(async () => {
    try {
      const res = await fetch("/api/nav", { cache: "no-store" });
      // An expired token is redirected through /api/auth/expired → /login.
      // Full-page replace, not router.push: it drops the client router
      // cache of the signed-in user's pages and leaves no dead history
      // entry behind.
      if (res.redirected && new URL(res.url).pathname === "/login") {
        window.location.replace(res.url);
        return;
      }
      if (res.status === 401) {
        window.location.replace("/login?reason=expired");
        return;
      }
      if (res.ok) setStatus((await res.json()) as NavStatus);
    } catch {
      /* offline — keep the last known status */
    }
  }, []);

  useEffect(() => {
    const first = setTimeout(load, 0);
    const timer = setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, 60_000);
    const onFocus = () => void load();
    window.addEventListener("focus", onFocus);
    return () => {
      clearTimeout(first);
      clearInterval(timer);
      window.removeEventListener("focus", onFocus);
    };
  }, [load]);

  return <Ctx.Provider value={status}>{children}</Ctx.Provider>;
}

export function useNavStatus(): NavStatus | null {
  return useContext(Ctx);
}
