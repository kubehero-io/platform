// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Shell status for the sidebar / footer / ⌘K (lib/api/nav.ts).

import { getNavStatus } from "@/lib/api/nav";
import { unauthorized } from "@/lib/api/guard";
import { getSession } from "@/lib/session";

export const dynamic = "force-dynamic";

export async function GET() {
  if (!(await getSession())) return unauthorized();
  const status = await getNavStatus();
  return Response.json(status, { headers: { "Cache-Control": "no-store" } });
}
