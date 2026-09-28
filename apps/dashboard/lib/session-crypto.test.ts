// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { createHmac } from "node:crypto";
import { describe, expect, it } from "vitest";
import { isSession, signSession, verifySession } from "./session-crypto";
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
