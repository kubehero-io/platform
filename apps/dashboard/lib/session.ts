// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Session: a single encrypted, HMAC-signed cookie (lib/session-crypto.ts).
// No server-side store.
//
// Two flavours, chosen by lib/auth-mode.ts:
//   demo   any email, no password — the product tour, only without a
//          control plane behind the dashboard.
//   token  the user signed in with a control-plane credential that
//          WhoAmI validated; the cookie carries that credential so every
//          server-side RPC runs AS the user. The token never reaches
//          client components — use getSessionView() for anything that is
//          rendered.
//
// All reads/writes happen server-side (server components, server
// actions, route handlers); the cookie is httpOnly.

import { cookies } from "next/headers";
import { sessionAcceptable } from "./auth-gate";
import { authMode, type AuthMode } from "./auth-mode";
import type { Role } from "./roles";
import { signSession, verifySession } from "./session-crypto";

export type Session = {
  email: string;
  org: string;
  onboarded: boolean;
  createdAt: number;
  /** Absent on sessions minted before auth modes existed → demo. */
  mode?: AuthMode;
  /** Token mode: the user's control-plane credential. Server-only. */
  token?: string;
  role?: Role;
  /** WhoAmI subject — key hash, OIDC sub, or "anonymous". */
  subject?: string;
  /** Epoch ms after which the session is refused. */
  expiresAt?: number;
};

/** What UI code may see: everything except the credential. */
export type SessionView = Omit<Session, "token"> & { hasToken: boolean };

export const SESSION_COOKIE = "kh_session";
const DEMO_MAX_AGE = 60 * 60 * 24 * 7; // 7 days
export const TOKEN_MAX_AGE = 60 * 60 * 12; // 12 hours — it holds a credential

export async function getSession(): Promise<Session | null> {
  let raw: string | undefined;
  try {
    const c = await cookies();
    raw = c.get(SESSION_COOKIE)?.value;
  } catch {
    // Outside a request scope (e.g. unit tests of the RPC layer): no user.
    return null;
  }
  if (!raw) return null;
  const s = verifySession(raw);
  if (!s || !sessionAcceptable(s, authMode())) return null;
  return s;
}

export async function getSessionView(): Promise<SessionView | null> {
  const s = await getSession();
  if (!s) return null;
  const { token, ...rest } = s;
  return { ...rest, hasToken: !!token };
}

export async function setSession(patch: Partial<Session>, opts: { maxAgeSec?: number } = {}) {
  const current = (await getSession()) ?? {
    email: "",
    org: "",
    onboarded: false,
    createdAt: Date.now(),
  };
  const next: Session = { ...current, ...patch };
  const c = await cookies();
  c.set(SESSION_COOKIE, signSession(next), {
    path: "/",
    httpOnly: true,
    secure: process.env.NODE_ENV === "production",
    sameSite: "lax",
    maxAge: opts.maxAgeSec ?? DEMO_MAX_AGE,
  });
}

export async function clearSession() {
  const c = await cookies();
  c.set(SESSION_COOKIE, "", { path: "/", maxAge: 0 });
}

export function orgFromEmail(email: string): string {
  const at = email.indexOf("@");
  if (at === -1) return "demo";
  const domain = email.slice(at + 1).split(".")[0] ?? "demo";
  return domain.toLowerCase();
}
