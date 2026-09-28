// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Session checks for server actions and route handlers. proxy.ts only
// gates page navigations (its matcher skips /api), and server actions
// are POSTs any client can forge — so every action and handler asks
// again here. The control plane re-checks roles regardless; this layer
// exists to fail fast with a clear message and to keep demo mode from
// pretending to persist anything.

import "server-only";
import { authMode, type AuthMode } from "@/lib/auth-mode";
import { canAdmin } from "@/lib/roles";
import { getSession, type Session } from "@/lib/session";

export type ActionError = {
  code: "unauthenticated" | "permission_denied" | "invalid_argument" | "demo" | "upstream";
  message: string;
};

export type ActionResult<T = void> =
  | { ok: true; data?: T; demo?: boolean; note?: string }
  | { ok: false; error: ActionError };

export async function currentSession(): Promise<{ session: Session; mode: AuthMode } | null> {
  const session = await getSession();
  return session ? { session, mode: authMode() } : null;
}

/** For admin-only mutations (ArmPolicy, alert rules, silences). */
export async function requireAdmin(): Promise<
  { ok: true; session: Session; mode: AuthMode } | { ok: false; error: ActionError }
> {
  const cur = await currentSession();
  if (!cur) return { ok: false, error: { code: "unauthenticated", message: "Sign in again." } };
  if (!canAdmin(cur.session.role ?? (cur.mode === "demo" ? "admin" : undefined))) {
    return {
      ok: false,
      error: {
        code: "permission_denied",
        message: `Admin role required — you are signed in as ${cur.session.role ?? "viewer"}.`,
      },
    };
  }
  return { ok: true, ...cur };
}

/** For route handlers: a JSON 401 instead of an HTML redirect. */
export function unauthorized(): Response {
  return Response.json({ error: { code: "unauthenticated", message: "sign in required" } }, { status: 401 });
}
