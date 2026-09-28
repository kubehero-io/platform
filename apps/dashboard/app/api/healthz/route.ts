// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Liveness/readiness for the pod: answers from this process alone. Probing
// /login instead would couple the dashboard's readiness to the control
// plane (token mode renders /login after a WhoAmI round trip) and take the
// sign-in page down exactly when users need its "unreachable" message.
// Unauthenticated by design, so it says nothing about configuration.

export const dynamic = "force-dynamic";

export function GET() {
  return Response.json({ status: "ok" }, { headers: { "Cache-Control": "no-store" } });
}
