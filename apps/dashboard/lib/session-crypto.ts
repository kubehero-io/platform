// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Session cookie codec. Pure functions, no Next.js imports — so
// lib/session.ts stays a thin cookie wrapper and this logic is
// unit-testable in isolation.
//
// In token mode the session carries the user's control-plane credential,
// so the payload is ENCRYPTED, not just signed:
//
//   cookie  = base64url(sealed) "." base64url(HMAC-SHA256(macKey, base64url(sealed)))
//   sealed  = 0x02 ‖ iv(12) ‖ AES-256-GCM(encKey, json, aad="kh_session") ‖ tag(16)
//
// Both keys are derived from KUBEHERO_SESSION_SECRET with HKDF-SHA256
// under distinct `info` labels. GCM already authenticates the
// ciphertext; the outer HMAC keeps the historical "payload.signature"
// wire shape (and its constant-time reject path) so a tampered cookie is
// refused before any decryption work happens.
//
// v1 cookies (plain base64 JSON + HMAC) are rejected: they may predate
// the encrypted format and nobody should be able to replay one after an
// upgrade. Users simply sign in again.

import {
  createCipheriv,
  createDecipheriv,
  createHmac,
  hkdfSync,
  randomBytes,
  timingSafeEqual,
} from "node:crypto";
import type { Session } from "./session";

// DEV FALLBACK — NOT A SECRET. Used only when KUBEHERO_SESSION_SECRET is
// unset so local dev works out of the box. Every real deployment must set
// KUBEHERO_SESSION_SECRET; production logs a loud warning without it.
const DEV_FALLBACK_SECRET = "kubehero-insecure-dev-secret-set-KUBEHERO_SESSION_SECRET";

const VERSION = 0x02;
const IV_BYTES = 12;
const TAG_BYTES = 16;
const AAD = Buffer.from("kh_session", "utf8");
const HKDF_SALT = Buffer.from("kubehero.session.v2", "utf8");
// Browsers cap a cookie at ~4 KiB; refuse to even parse anything larger.
const MAX_COOKIE_CHARS = 4096;

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

type Keys = { enc: Buffer; mac: Buffer };
const keyCache = new Map<string, Keys>();

function deriveKeys(secret: string): Keys {
  const hit = keyCache.get(secret);
  if (hit) return hit;
  const ikm = Buffer.from(secret, "utf8");
  const keys: Keys = {
    enc: Buffer.from(hkdfSync("sha256", ikm, HKDF_SALT, "aes-256-gcm", 32)),
    mac: Buffer.from(hkdfSync("sha256", ikm, HKDF_SALT, "hmac-sha256", 32)),
  };
  // Secrets rarely change; the cap only guards pathological test churn.
  if (keyCache.size > 8) keyCache.clear();
  keyCache.set(secret, keys);
  return keys;
}

export function signSession(session: Session, key: string = sessionSecret()): string {
  const { enc, mac } = deriveKeys(key);
  const iv = randomBytes(IV_BYTES);
  const cipher = createCipheriv("aes-256-gcm", enc, iv);
  cipher.setAAD(AAD);
  const ct = Buffer.concat([cipher.update(JSON.stringify(session), "utf8"), cipher.final()]);
  const sealed = Buffer.concat([Buffer.from([VERSION]), iv, ct, cipher.getAuthTag()]);
  const payload = sealed.toString("base64url");
  const sig = createHmac("sha256", mac).update(payload).digest("base64url");
  return `${payload}.${sig}`;
}

// Invalid, tampered, legacy (v1 / unsigned) or undecryptable cookies all
// return null — callers treat that as logged-out.
export function verifySession(raw: string, key: string = sessionSecret()): Session | null {
  if (!raw || raw.length > MAX_COOKIE_CHARS) return null;
  const parts = raw.split(".");
  if (parts.length !== 2) return null;
  const [payload, sig] = parts;
  if (!payload || !sig) return null;

  const { enc, mac } = deriveKeys(key);
  const expected = createHmac("sha256", mac).update(payload).digest();
  const given = Buffer.from(sig, "base64url");
  if (given.length !== expected.length || !timingSafeEqual(given, expected)) {
    return null;
  }

  const sealed = Buffer.from(payload, "base64url");
  if (sealed.length < 1 + IV_BYTES + TAG_BYTES + 2 || sealed[0] !== VERSION) return null;
  const iv = sealed.subarray(1, 1 + IV_BYTES);
  const tag = sealed.subarray(sealed.length - TAG_BYTES);
  const ct = sealed.subarray(1 + IV_BYTES, sealed.length - TAG_BYTES);
  try {
    const decipher = createDecipheriv("aes-256-gcm", enc, iv);
    decipher.setAAD(AAD);
    decipher.setAuthTag(tag);
    const json = Buffer.concat([decipher.update(ct), decipher.final()]).toString("utf8");
    const parsed: unknown = JSON.parse(json);
    return isSession(parsed) ? parsed : null;
  } catch {
    return null;
  }
}

const ROLES = new Set(["anonymous", "viewer", "auditor", "member", "admin", "owner"]);

export function isSession(v: unknown): v is Session {
  if (typeof v !== "object" || v === null) return false;
  const s = v as Record<string, unknown>;
  const base =
    typeof s.email === "string" &&
    typeof s.org === "string" &&
    typeof s.onboarded === "boolean" &&
    typeof s.createdAt === "number";
  if (!base) return false;
  // Optional token-mode fields must have the right types when present.
  if (s.mode !== undefined && s.mode !== "demo" && s.mode !== "token") return false;
  if (s.token !== undefined && typeof s.token !== "string") return false;
  if (s.subject !== undefined && typeof s.subject !== "string") return false;
  if (s.expiresAt !== undefined && typeof s.expiresAt !== "number") return false;
  if (s.role !== undefined && (typeof s.role !== "string" || !ROLES.has(s.role))) return false;
  return true;
}
