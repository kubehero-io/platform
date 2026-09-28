// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// localStorage as an external store (useSyncExternalStore): SSR renders
// the fallback, the client reads the real value without a setState-in-
// effect round trip, and every hook instance (and other tabs, via the
// `storage` event) stays in sync. Values are validated on read — the
// browser's storage is user-controlled input.

import { useCallback, useMemo, useSyncExternalStore } from "react";

const EVENT = "kh:local-storage";

function subscribe(cb: () => void): () => void {
  window.addEventListener("storage", cb);
  window.addEventListener(EVENT, cb);
  return () => {
    window.removeEventListener("storage", cb);
    window.removeEventListener(EVENT, cb);
  };
}

// When the browser refuses writes (blocked storage, quota), values live
// here for the page's lifetime so the UI still behaves — just unpersisted.
const memory = new Map<string, string | null>();

function read(key: string): string | null {
  if (memory.has(key)) return memory.get(key) ?? null;
  try {
    return localStorage.getItem(key);
  } catch {
    return null; // private mode / blocked storage
  }
}

/** Returns false when the browser refused the write (quota, private mode). */
export function writeLocalStorage(key: string, value: string | null): boolean {
  let ok = true;
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
    memory.delete(key);
  } catch {
    ok = false; // the UI keeps working without persistence
    memory.set(key, value);
  }
  window.dispatchEvent(new Event(EVENT));
  return ok;
}

/** One-off read of a validated JSON value (event handlers, callbacks). */
export function readLocalStorageJSON<T>(key: string, fallback: T, validate: (v: unknown) => T | null): T {
  const raw = read(key);
  if (!raw) return fallback;
  try {
    return validate(JSON.parse(raw)) ?? fallback;
  } catch {
    return fallback;
  }
}

/** The raw string (stable snapshot, so no render loops). */
export function useLocalStorageString(key: string): string | null {
  return useSyncExternalStore(
    subscribe,
    () => read(key),
    () => null,
  );
}

/**
 * Parsed + validated JSON value and a setter. `fallback` must be a
 * stable reference (module constant).
 */
export function useLocalStorageJSON<T>(key: string, fallback: T, validate: (v: unknown) => T | null): [T, (next: T) => void] {
  const raw = useLocalStorageString(key);
  const value = useMemo(() => {
    if (!raw) return fallback;
    try {
      return validate(JSON.parse(raw)) ?? fallback;
    } catch {
      return fallback;
    }
  }, [raw, fallback, validate]);
  const set = useCallback((next: T) => void writeLocalStorage(key, JSON.stringify(next)), [key]);
  return [value, set];
}
