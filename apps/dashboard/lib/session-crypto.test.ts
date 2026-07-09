// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { signSession, verifySession } from "./session-crypto";
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
