// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { NextResponse } from "next/server";
import { clearSession } from "@/lib/session";

// POST only: a GET logout is a CSRF-able side effect. SameSite=lax on
// the session cookie already keeps cross-site POSTs from carrying it.
export async function POST() {
  await clearSession();
  return NextResponse.json({ ok: true });
}
