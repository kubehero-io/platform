// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { NextResponse, type NextRequest } from "next/server";

const AUTH_ROUTES = new Set(["/login", "/signup"]);
const PROTECTED_PREFIXES = [
  "/overview",
  "/advisor",
  "/fleet",
  "/clusters",
  "/waste",
  "/workloads",
  "/capacity",
  "/gpu",
  "/budgets",
  "/ceilings",
  "/chargeback",
  "/posture",
  "/settings",
  "/onboarding",
];

export function middleware(req: NextRequest) {
  const { pathname } = req.nextUrl;
  const hasSession = req.cookies.get("kh_session")?.value;

  const isAuth = AUTH_ROUTES.has(pathname);
  const isProtected = PROTECTED_PREFIXES.some(
    (p) => pathname === p || pathname.startsWith(p + "/"),
  );

  // Authed users bounce away from /login.
  if (hasSession && isAuth) {
    return NextResponse.redirect(new URL("/overview", req.url));
  }

  // Unauthed users bounce away from dashboard routes.
  if (!hasSession && isProtected) {
    const url = new URL("/login", req.url);
    url.searchParams.set("next", pathname);
    return NextResponse.redirect(url);
  }

  // Onboarding gate: if session exists but onboarded=false and they're
  // heading anywhere except /onboarding, nudge them through the wizard.
  //
  // The cookie is base64url(payload).base64url(hmac) — see lib/session-crypto.ts.
  // Middleware runs on the edge runtime (no node:crypto), so it only *peeks*
  // at the payload for this UX redirect. Signature verification happens
  // server-side in getSession(), which treats tampered cookies as logged-out.
  if (hasSession && isProtected && pathname !== "/onboarding") {
    try {
      const payload = hasSession.split(".")[0] ?? "";
      const json = atob(payload.replace(/-/g, "+").replace(/_/g, "/"));
      const s = JSON.parse(json) as { onboarded?: boolean };
      if (!s.onboarded) {
        return NextResponse.redirect(new URL("/onboarding", req.url));
      }
    } catch {
      /* bad cookie → let the page handle it */
    }
  }

  return NextResponse.next();
}

export const config = {
  matcher: ["/((?!_next/|api/|favicon|.*\\..*).*)"],
};
