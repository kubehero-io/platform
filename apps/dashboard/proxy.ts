// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Next 16 renamed middleware → proxy and runs it on the Node.js runtime,
// which is what lets us decrypt + verify the session here (policy in
// lib/auth-gate.ts, codec in lib/session-crypto.ts).
//
// API routes are excluded by the matcher on purpose: route handlers
// answer 401 JSON themselves (lib/api/guard.ts) instead of redirecting a
// fetch() to an HTML login page.

import { NextResponse, type NextRequest } from "next/server";
import { gate } from "@/lib/auth-gate";
import { authMode } from "@/lib/auth-mode";
import { verifySession } from "@/lib/session-crypto";

const SESSION_COOKIE = "kh_session";

export function proxy(req: NextRequest) {
  const decision = gate(
    req.nextUrl.pathname,
    req.cookies.get(SESSION_COOKIE)?.value,
    (raw) => verifySession(raw),
    authMode(),
  );
  const res =
    decision.kind === "next"
      ? NextResponse.next()
      : NextResponse.redirect(new URL(decision.to, req.url));
  if (decision.clearCookie) res.cookies.set(SESSION_COOKIE, "", { path: "/", maxAge: 0 });
  return res;
}

export const config = {
  matcher: ["/((?!_next/|api/|favicon|.*\\..*).*)"],
};
