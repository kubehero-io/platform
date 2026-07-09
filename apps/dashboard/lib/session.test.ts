// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { beforeEach, describe, expect, it, vi } from "vitest";

// In-memory cookie jar standing in for next/headers.
const jar = vi.hoisted(
  () => new Map<string, { value: string; options?: Record<string, unknown> }>(),
);

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => {
      const entry = jar.get(name);
      return entry ? { name, value: entry.value } : undefined;
    },
    set: (name: string, value: string, options?: Record<string, unknown>) => {
      jar.set(name, { value, options });
    },
  }),
}));

import {
  SESSION_COOKIE,
  clearSession,
  getSession,
  orgFromEmail,
  setSession,
} from "./session";

beforeEach(() => {
  jar.clear();
});

describe("setSession / getSession", () => {
  it("round-trips through the cookie", async () => {
    await setSession({ email: "ada@acme.io", org: "acme", onboarded: true });
    const s = await getSession();
    expect(s).toMatchObject({ email: "ada@acme.io", org: "acme", onboarded: true });
    expect(typeof s?.createdAt).toBe("number");
  });

  it("sets a hardened, signed cookie", async () => {
    await setSession({ email: "ada@acme.io" });
    const entry = jar.get(SESSION_COOKIE);
    expect(entry).toBeDefined();
    expect(entry?.options).toMatchObject({
      httpOnly: true,
      sameSite: "lax",
      path: "/",
      secure: false, // NODE_ENV !== "production" under vitest
    });
    // Signed token, not plaintext JSON.
    expect(entry?.value.split(".")).toHaveLength(2);
    expect(entry?.value).not.toContain("{");
  });

  it("merges a patch into the existing session", async () => {
    await setSession({ email: "ada@acme.io", org: "acme", onboarded: false });
    await setSession({ onboarded: true });
    const s = await getSession();
    expect(s).toMatchObject({ email: "ada@acme.io", org: "acme", onboarded: true });
  });

  it("treats a tampered cookie as logged-out", async () => {
    await setSession({ email: "ada@acme.io", org: "acme" });
    const entry = jar.get(SESSION_COOKIE)!;
    const [, sig] = entry.value.split(".");
    const forged = Buffer.from(
      JSON.stringify({ email: "mallory@evil.io", org: "evil", onboarded: true, createdAt: 1 }),
      "utf8",
    ).toString("base64url");
    jar.set(SESSION_COOKIE, { value: `${forged}.${sig}` });
    expect(await getSession()).toBeNull();
  });

  it("treats a legacy plaintext JSON cookie as logged-out", async () => {
    jar.set(SESSION_COOKIE, {
      value: JSON.stringify({ email: "a@b.c", org: "b", onboarded: true, createdAt: 1 }),
    });
    expect(await getSession()).toBeNull();
  });

  it("returns null when no cookie is set", async () => {
    expect(await getSession()).toBeNull();
  });
});

describe("clearSession", () => {
  it("expires the cookie", async () => {
    await setSession({ email: "ada@acme.io" });
    await clearSession();
    const entry = jar.get(SESSION_COOKIE);
    expect(entry?.value).toBe("");
    expect(entry?.options).toMatchObject({ maxAge: 0 });
    expect(await getSession()).toBeNull();
  });
});

describe("orgFromEmail", () => {
  it("takes the first label of the domain", () => {
    expect(orgFromEmail("ada@acme.io")).toBe("acme");
    expect(orgFromEmail("dev@sub.example.co.uk")).toBe("sub");
  });

  it("lower-cases the org", () => {
    expect(orgFromEmail("ada@ACME.io")).toBe("acme");
  });

  it("falls back to demo without an @", () => {
    expect(orgFromEmail("not-an-email")).toBe("demo");
  });
});
