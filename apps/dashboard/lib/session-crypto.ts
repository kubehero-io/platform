// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// HMAC-signed session codec. Pure functions, no Next.js imports — so
// lib/session.ts stays a thin cookie wrapper and this logic is
// unit-testable in isolation.
//
// Wire format:  base64url(JSON payload) "." base64url(HMAC-SHA256(payload))
//
// The payload is readable (middleware peeks at `onboarded` for the
// onboarding redirect) but not forgeable without the key: getSession()
// rejects any cookie whose signature fails to verify.

import { createHmac, timingSafeEqual } from "node:crypto";
import type { Session } from "./session";

// DEV FALLBACK — NOT A SECRET. Used only when KUBEHERO_SESSION_SECRET is
// unset so local dev works out of the box. Every real deployment must set
// KUBEHERO_SESSION_SECRET; production logs a loud warning without it.
const DEV_FALLBACK_SECRET = "kubehero-insecure-dev-secret-set-KUBEHERO_SESSION_SECRET";

let warnedMissingSecret = false;

export function sessionSecret(): string {
  const s = process.env.KUBEHERO_SESSION_SECRET?.trim();
  if (s) return s;
  if (process.env.NODE_ENV === "production" && !warnedMissingSecret) {
    console.warn(
      "[session] KUBEHERO_SESSION_SECRET is not set — falling back to the insecure dev key. Set it in production.",
    );
    warnedMissingSecret = true;
  }
  return DEV_FALLBACK_SECRET;
}

export function signSession(session: Session, key: string = sessionSecret()): string {
  const payload = Buffer.from(JSON.stringify(session), "utf8").toString("base64url");
  const sig = createHmac("sha256", key).update(payload).digest("base64url");
  return `${payload}.${sig}`;
}

// Invalid, tampered, or legacy (unsigned) cookies all return null —
// callers treat that as logged-out.
export function verifySession(raw: string, key: string = sessionSecret()): Session | null {
  const parts = raw.split(".");
  if (parts.length !== 2) return null;
  const [payload, sig] = parts;
  const expected = createHmac("sha256", key).update(payload).digest();
  const given = Buffer.from(sig, "base64url");
  if (given.length !== expected.length || !timingSafeEqual(given, expected)) {
    return null;
  }
  try {
    const parsed: unknown = JSON.parse(Buffer.from(payload, "base64url").toString("utf8"));
    return isSession(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

function isSession(v: unknown): v is Session {
  if (typeof v !== "object" || v === null) return false;
  const s = v as Record<string, unknown>;
  return (
    typeof s.email === "string" &&
    typeof s.org === "string" &&
    typeof s.onboarded === "boolean" &&
    typeof s.createdAt === "number"
  );
}
