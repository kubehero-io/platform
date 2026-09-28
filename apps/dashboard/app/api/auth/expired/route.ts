// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Landing spot when the control plane rejects the signed-in user's token
// mid-session (revoked key, expired OIDC token). A page can't clear
// cookies while rendering, and bouncing straight to /login would loop —
// the cookie is still cryptographically valid, so proxy.ts would send the
// user right back. Route handlers may set cookies: clear it here, then
// continue to the sign-in page.

import { NextResponse } from "next/server";
import { SESSION_COOKIE } from "@/lib/session";

export function GET() {
  // A relative Location on purpose: behind an ingress, a route handler's
  // request URL carries the pod's own address (e.g. localhost:3001), not
  // the host the browser used.
  const res = new NextResponse(null, { status: 307, headers: { Location: "/login?reason=expired", "Cache-Control": "no-store" } });
  res.cookies.set(SESSION_COOKIE, "", { path: "/", maxAge: 0 });
  return res;
}
