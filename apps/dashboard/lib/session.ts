// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Demo-mode session: a single JSON-encoded cookie. No server-side store,
// no password, no JWT. It's here to shape the UX — arriving design
// partners experience a real login → onboarding → dashboard flow without
// us having to wire a real auth provider first.
//
// When we ship real auth (Clerk / WorkOS / Dex), this file is the only
// seam that changes.

import { cookies } from "next/headers";

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
  try {
    return JSON.parse(raw) as Session;
  } catch {
    return null;
  }
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
  c.set(SESSION_COOKIE, JSON.stringify(next), {
    path: "/",
    httpOnly: false, // demo-mode: client code reads it too
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
