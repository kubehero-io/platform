// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Pure routing decision for proxy.ts (the Next 16 successor of
// middleware.ts). Kept free of Next imports so the policy is unit-tested
// in isolation.
//
// Policy — default deny: every page route needs a valid session except
// the sign-in pages themselves. The old middleware kept an allow-list of
// protected prefixes, which silently left every newly added page
// (/logs, /alerts, …) public until someone remembered to extend it.
//
// Proxy runs on the Node.js runtime in Next 16 (edge is not supported
// for proxy), so unlike the old edge middleware it verifies and decrypts
// the cookie instead of only peeking at the payload. A forged, expired
// or wrong-mode cookie is treated exactly like no cookie.

import type { AuthMode } from "./auth-mode";
import type { Session } from "./session";

export type GateDecision =
  | { kind: "next"; clearCookie?: boolean }
  | { kind: "redirect"; to: string; clearCookie?: boolean };

/**
 * Whether a cryptographically valid session is acceptable right now:
 * minted under the auth mode the dashboard currently runs in (a demo
 * session must not survive a switch to token mode) and not expired.
 */
export function sessionAcceptable(s: Session, mode: AuthMode, now = Date.now()): boolean {
  const sessionMode = s.mode ?? "demo";
  if (sessionMode !== mode) return false;
  if (s.expiresAt !== undefined && s.expiresAt <= now) return false;
  return true;
}

/** Sign-in routes reachable without a session. Sign-up is a demo-only tour. */
export function authRoutes(mode: AuthMode): ReadonlySet<string> {
  return mode === "demo" ? new Set(["/login", "/signup"]) : new Set(["/login"]);
}

export function gate(
  pathname: string,
  rawCookie: string | undefined,
  verify: (raw: string) => Session | null,
  mode: AuthMode = "demo",
  now = Date.now(),
): GateDecision {
  const verified = rawCookie ? verify(rawCookie) : null;
  const session = verified && sessionAcceptable(verified, mode, now) ? verified : null;
  // A cookie was presented but is unusable — drop it so the browser
  // stops replaying it.
  const clearCookie = !!rawCookie && !session;

  // Token mode has no sign-up tour and no onboarding wizard.
  if (mode === "token" && (pathname === "/signup" || pathname === "/onboarding")) {
    return { kind: "redirect", to: session ? "/overview" : "/login", clearCookie };
  }

  if (authRoutes(mode).has(pathname)) {
    if (session) return { kind: "redirect", to: "/overview" };
    return clearCookie ? { kind: "next", clearCookie } : { kind: "next" };
  }

  if (!session) {
    const next = safeNextPath(pathname);
    const to = pathname === "/" || next !== pathname ? "/login" : `/login?next=${encodeURIComponent(next)}`;
    return { kind: "redirect", to, clearCookie };
  }

  // Onboarding gate: new demo accounts go through the wizard first.
  if (!session.onboarded && pathname !== "/onboarding" && mode === "demo") {
    return { kind: "redirect", to: "/onboarding" };
  }
  return { kind: "next" };
}

/**
 * Only same-origin absolute paths survive as a post-login redirect
 * target. Rejects protocol-relative ("//evil.com") and backslash tricks
 * so /login?next= cannot become an open redirect.
 */
export function safeNextPath(p: string | null | undefined): string {
  if (!p || typeof p !== "string") return "/overview";
  if (!p.startsWith("/") || p.startsWith("//") || p.includes("\\")) return "/overview";
  if (p.length > 512) return "/overview";
  return p;
}
