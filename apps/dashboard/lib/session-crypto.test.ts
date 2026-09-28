// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { createHmac } from "node:crypto";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  isSession,
  SessionSecretMissingError,
  sessionSecret,
  sessionSecretConfigured,
  signSession,
  verifySession,
} from "./session-crypto";
import type { Session } from "./session";

const KEY = "test-secret-key";

const SESSION: Session = {
  email: "ada@acme.io",
  org: "acme",
  onboarded: true,
  createdAt: 1_750_000_000_000,
};

describe("signSession / verifySession", () => {
  it("round-trips a session", () => {
    const token = signSession(SESSION, KEY);
    expect(verifySession(token, KEY)).toEqual(SESSION);
  });

  it("round-trips with the default (dev fallback) key", () => {
    const token = signSession(SESSION);
    expect(verifySession(token)).toEqual(SESSION);
  });

  it("produces payload.signature with no plaintext JSON", () => {
    const token = signSession(SESSION, KEY);
    expect(token.split(".")).toHaveLength(2);
    expect(token).not.toContain("{");
    expect(token).not.toContain(SESSION.email);
  });

  it("rejects a tampered payload", () => {
    const token = signSession(SESSION, KEY);
    const [, sig] = token.split(".");
    const forged = Buffer.from(
      JSON.stringify({ ...SESSION, email: "mallory@evil.io" }),
      "utf8",
    ).toString("base64url");
    expect(verifySession(`${forged}.${sig}`, KEY)).toBeNull();
  });

  it("rejects a tampered signature", () => {
    const token = signSession(SESSION, KEY);
    const [payload, sig] = token.split(".");
    const flipped = (sig[0] === "A" ? "B" : "A") + sig.slice(1);
    expect(verifySession(`${payload}.${flipped}`, KEY)).toBeNull();
    expect(verifySession(`${payload}.${sig.slice(0, -4)}`, KEY)).toBeNull();
  });

  it("rejects a token signed with a different key", () => {
    const token = signSession(SESSION, "other-key");
    expect(verifySession(token, KEY)).toBeNull();
  });

  it("rejects legacy plaintext JSON cookies", () => {
    expect(verifySession(JSON.stringify(SESSION), KEY)).toBeNull();
  });

  it("rejects garbage", () => {
    expect(verifySession("", KEY)).toBeNull();
    expect(verifySession("not-a-token", KEY)).toBeNull();
    expect(verifySession("a.b.c", KEY)).toBeNull();
    expect(verifySession("....", KEY)).toBeNull();
  });

  it("rejects a correctly signed payload with the wrong shape", () => {
    const token = signSession({ email: "x@y.z" } as Session, KEY);
    expect(verifySession(token, KEY)).toBeNull();
  });
});

describe("encrypted token sessions", () => {
  const TOKEN_SESSION: Session = {
    ...SESSION,
    mode: "token",
    token: "kh_live_4f9c2d7a0b1e8c3d5a6f7e8d9c0b1a2f",
    role: "admin",
    subject: "key:4f9c2d7a",
    expiresAt: 1_750_043_200_000,
  };

  it("round-trips every token-mode field", () => {
    expect(verifySession(signSession(TOKEN_SESSION, KEY), KEY)).toEqual(TOKEN_SESSION);
  });

  it("never leaks the credential (or any field) into the cookie", () => {
    const cookie = signSession(TOKEN_SESSION, KEY);
    expect(cookie).not.toContain(TOKEN_SESSION.token);
    expect(cookie).not.toContain(Buffer.from(TOKEN_SESSION.token!).toString("base64url").slice(0, 16));
    expect(cookie).not.toContain("admin");
  });

  it("uses a fresh IV per cookie", () => {
    expect(signSession(TOKEN_SESSION, KEY)).not.toBe(signSession(TOKEN_SESSION, KEY));
  });

  it("rejects a v1 (signed-only, plaintext payload) cookie", () => {
    // The pre-encryption format: base64url(JSON) "." HMAC(secret, payload).
    const payload = Buffer.from(JSON.stringify(SESSION), "utf8").toString("base64url");
    const sig = createHmac("sha256", KEY).update(payload).digest("base64url");
    expect(verifySession(`${payload}.${sig}`, KEY)).toBeNull();
  });

  it("rejects oversized cookies without parsing them", () => {
    expect(verifySession("a".repeat(5000) + ".b", KEY)).toBeNull();
  });
});

describe("isSession", () => {
  it("rejects wrongly typed optional fields", () => {
    expect(isSession({ ...SESSION, mode: "root" })).toBe(false);
    expect(isSession({ ...SESSION, role: "superuser" })).toBe(false);
    expect(isSession({ ...SESSION, token: 42 })).toBe(false);
    expect(isSession({ ...SESSION, expiresAt: "soon" })).toBe(false);
    expect(isSession({ ...SESSION, mode: "token", role: "viewer", token: "x" })).toBe(true);
  });
});

describe("sessionSecret", () => {
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.restoreAllMocks();
  });

  it("uses KUBEHERO_SESSION_SECRET when set", () => {
    expect(sessionSecret({ KUBEHERO_SESSION_SECRET: " s3cret ", NODE_ENV: "production", KUBEHERO_DASHBOARD_AUTH: "token" })).toBe("s3cret");
  });

  it("refuses the published dev key in production token mode", () => {
    expect(() => sessionSecret({ NODE_ENV: "production", KUBEHERO_DASHBOARD_AUTH: "token" })).toThrow(SessionSecretMissingError);
    // Token mode implied by a configured control plane.
    expect(() => sessionSecret({ NODE_ENV: "production", CONTROL_PLANE_URL: "http://cp:8080" })).toThrow(SessionSecretMissingError);
    expect(sessionSecretConfigured({ NODE_ENV: "production", CONTROL_PLANE_URL: "http://cp:8080" })).toBe(false);
  });

  it("allows the dev key for a production demo (with a warning) and in development", () => {
    const warn = vi.spyOn(console, "warn").mockImplementation(() => {});
    expect(sessionSecretConfigured({ NODE_ENV: "production" })).toBe(true);
    expect(warn).toHaveBeenCalledTimes(1);
    expect(sessionSecretConfigured({ NODE_ENV: "development", KUBEHERO_DASHBOARD_AUTH: "token" })).toBe(true);
  });

  it("treats every cookie as signed-out when the secret is missing in production token mode", () => {
    const cookie = signSession(SESSION, "whatever");
    vi.stubEnv("NODE_ENV", "production");
    vi.stubEnv("KUBEHERO_DASHBOARD_AUTH", "token");
    vi.stubEnv("KUBEHERO_SESSION_SECRET", "");
    expect(() => signSession(SESSION)).toThrow(SessionSecretMissingError);
    expect(verifySession(cookie)).toBeNull();
    // Even one minted with the published fallback key.
    expect(verifySession(signSession(SESSION, "kubehero-insecure-dev-secret-set-KUBEHERO_SESSION_SECRET"))).toBeNull();
  });
});
