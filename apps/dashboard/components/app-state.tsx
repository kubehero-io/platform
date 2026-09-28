// SPDX-License-Identifier: BUSL-1.1
"use client";

import { createContext, useCallback, useContext, useMemo } from "react";
import { readLocalStorageJSON, useLocalStorageJSON, writeLocalStorage } from "@/lib/use-local-storage";

/* Small client-only store. Persists per-org via localStorage so a
   reload keeps the demo state. Swap for React Query + real API later. */

type Event = {
  id: number;
  at: string;
  kind: "applied" | "armed" | "disarmed" | "reverted" | "recommended";
  policy?: string;
  workload?: string;
  cluster?: string;
  savingsK?: number;
  auditId: string;
};

type State = {
  applied: Record<string, boolean>;    // workload name → applied
  armed: Record<string, boolean>;      // policy name → armed
  events: Event[];
};

type Ctx = {
  state: State;
  apply: (workload: string, savingsK: number, cluster: string) => void;
  revert: (workload: string) => void;
  arm: (policy: string) => void;
  disarm: (policy: string) => void;
};

const AppCtx = createContext<Ctx | null>(null);

const KEY = "kh_demo_state_v1";

const defaults: State = {
  applied: {},
  armed: {
    "prod-monthly-ceiling": true,
    "ml-gpu-ceiling": true,
    "prod-burn-rate-2x": true,
    "gpu-inference-cap": false,
    "staging-monthly": false,
  },
  events: [],
};

function nowStr() {
  const d = new Date();
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
}

function validate(v: unknown): State | null {
  if (!v || typeof v !== "object") return null;
  const o = v as Partial<State>;
  return {
    applied: o.applied && typeof o.applied === "object" ? o.applied : {},
    armed: o.armed && typeof o.armed === "object" ? { ...defaults.armed, ...o.armed } : defaults.armed,
    events: Array.isArray(o.events) ? o.events.slice(0, 50) : [],
  };
}

// Functional update against the current stored value (not a render's
// snapshot), so rapid successive actions never overwrite each other.
function update(fn: (s: State) => State) {
  writeLocalStorage(KEY, JSON.stringify(fn(readLocalStorageJSON(KEY, defaults, validate))));
}

export function AppStateProvider({
  children,
}: {
  children: React.ReactNode;
}) {
  // Persisted per browser (demo interactions only — nothing here reaches
  // a control plane).
  const [state] = useLocalStorageJSON<State>(KEY, defaults, validate);

  const apply = useCallback(
    (workload: string, savingsK: number, cluster: string) => {
      const ev: Event = {
        id: Date.now(),
        at: nowStr(),
        kind: "applied",
        workload,
        cluster,
        savingsK,
        auditId: "aud-" + String(50000 + Math.floor(Math.random() * 9999)),
      };
      update((s) => ({
        ...s,
        applied: { ...s.applied, [workload]: true },
        events: [ev, ...s.events].slice(0, 50),
      }));
    },
    [],
  );

  const revert = useCallback((workload: string) => {
    const ev: Event = {
      id: Date.now(),
      at: nowStr(),
      kind: "reverted",
      workload,
      auditId: "aud-" + String(50000 + Math.floor(Math.random() * 9999)),
    };
    update((s) => ({
      ...s,
      applied: { ...s.applied, [workload]: false },
      events: [ev, ...s.events].slice(0, 50),
    }));
  }, []);

  const arm = useCallback((policy: string) => {
    const ev: Event = {
      id: Date.now(),
      at: nowStr(),
      kind: "armed",
      policy,
      auditId: "aud-" + String(50000 + Math.floor(Math.random() * 9999)),
    };
    update((s) => ({
      ...s,
      armed: { ...s.armed, [policy]: true },
      events: [ev, ...s.events].slice(0, 50),
    }));
  }, []);

  const disarm = useCallback((policy: string) => {
    const ev: Event = {
      id: Date.now(),
      at: nowStr(),
      kind: "disarmed",
      policy,
      auditId: "aud-" + String(50000 + Math.floor(Math.random() * 9999)),
    };
    update((s) => ({
      ...s,
      armed: { ...s.armed, [policy]: false },
      events: [ev, ...s.events].slice(0, 50),
    }));
  }, []);

  const value = useMemo(
    () => ({ state, apply, revert, arm, disarm }),
    [state, apply, revert, arm, disarm],
  );

  return <AppCtx.Provider value={value}>{children}</AppCtx.Provider>;
}

export function useAppState() {
  const c = useContext(AppCtx);
  if (!c) throw new Error("useAppState outside <AppStateProvider>");
  return c;
}

export type { Event };
