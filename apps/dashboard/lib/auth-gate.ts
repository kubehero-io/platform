// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Pure routing decision for proxy.ts (the Next 16 successor of
// middleware.ts). Kept free of Next imports so the policy is unit-tested
// in isolation.
//
// Policy — default deny: every page route needs a valid session except
// the auth pages themselves. The old middleware kept an allow-list of
// protected prefixes, which silently left every newly added page
// (/logs, /alerts, …) public until someone remembered to extend it.
//
// Proxy runs on the Node.js runtime in Next 16 (edge is not supported
// for proxy), so unlike the old edge middleware it can afford to verify
// the cookie's HMAC instead of only peeking at the payload. A forged or
// stale cookie is treated exactly like no cookie.

import type { Session } from "./session";

export const AUTH_ROUTES = new Set(["/login", "/signup"]);

export type GateDecision =
  | { kind: "next" }
  | { kind: "redirect"; to: string; clearCookie?: boolean };

export function gate(
  pathname: string,
  rawCookie: string | undefined,
  verify: (raw: string) => Session | null,
): GateDecision {
  const session = rawCookie ? verify(rawCookie) : null;
  const isAuthRoute = AUTH_ROUTES.has(pathname);

  if (isAuthRoute) {
    // Signed-in users have no business on /login.
    return session ? { kind: "redirect", to: "/overview" } : { kind: "next" };
  }

  if (!session) {
    const next = safeNextPath(pathname);
    return {
      kind: "redirect",
      to: next === "/" ? "/login" : `/login?next=${encodeURIComponent(next)}`,
      // A cookie was presented but failed verification — drop it so the
      // browser stops replaying it.
      clearCookie: rawCookie !== undefined && rawCookie !== "",
    };
  }

  // Onboarding gate: new accounts go through the wizard first.
  if (!session.onboarded && pathname !== "/onboarding") {
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
