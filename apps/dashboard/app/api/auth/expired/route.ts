// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Landing spot when the control plane rejects the signed-in user's token
// mid-session (revoked key, expired OIDC token). A page can't clear
// cookies while rendering, and bouncing straight to /login would loop —
// the cookie is still cryptographically valid, so proxy.ts would send the
// user right back. Route handlers may set cookies: clear it here, then
// continue to the sign-in page.

import { NextResponse, type NextRequest } from "next/server";
import { SESSION_COOKIE } from "@/lib/session";

export function GET(req: NextRequest) {
  const res = NextResponse.redirect(new URL("/login?reason=expired", req.url));
  res.cookies.set(SESSION_COOKIE, "", { path: "/", maxAge: 0 });
  return res;
}
