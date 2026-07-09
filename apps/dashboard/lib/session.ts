// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo-mode session: a single HMAC-signed cookie. No server-side store,
// no password, no JWT. It's here to shape the UX — arriving design
// partners experience a real login → onboarding → dashboard flow without
// us having to wire a real auth provider first.
//
// The cookie is httpOnly + signed (see lib/session-crypto.ts) so it can't
// be read or forged from the browser; invalid or tampered cookies read as
// logged-out. All reads/writes happen server-side (server components,
// server actions, route handlers).
//
// When we ship real auth (Clerk / WorkOS / Dex), this file is the only
// seam that changes.

import { cookies } from "next/headers";
import { signSession, verifySession } from "./session-crypto";

export type Session = {
  email: string;
  org: string;
  onboarded: boolean;
  createdAt: number;
};

export const SESSION_COOKIE = "kh_session";
const MAX_AGE = 60 * 60 * 24 * 7; // 7 days

export async function getSession(): Promise<Session | null> {
  const c = await cookies();
  const raw = c.get(SESSION_COOKIE)?.value;
  if (!raw) return null;
  return verifySession(raw);
}

export async function setSession(patch: Partial<Session>) {
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
    maxAge: MAX_AGE,
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
