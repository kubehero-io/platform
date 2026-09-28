// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const jar = vi.hoisted(() => new Map<string, { value: string; options?: Record<string, unknown> }>());
vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => (jar.has(name) ? { name, value: jar.get(name)!.value } : undefined),
    set: (name: string, value: string, options?: Record<string, unknown>) => jar.set(name, { value, options }),
  }),
}));
vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw Object.assign(new Error("NEXT_REDIRECT"), { to });
  },
}));

import { verifySession } from "@/lib/session-crypto";
import { signIn, signInAnonymous, signInWithToken } from "./actions";

const CP = "http://cp.test:18080";

function form(fields: Record<string, string>): FormData {
  const f = new FormData();
  for (const [k, v] of Object.entries(fields)) f.set(k, v);
  return f;
}

async function redirectOf(p: Promise<unknown>): Promise<string> {
  try {
    await p;
  } catch (e) {
    return (e as { to?: string }).to ?? "";
  }
  throw new Error("expected a redirect");
}

function stubWhoAmI(impl: (auth: string | undefined) => { status: number; body: unknown }) {
  const mock = vi.fn(async (_url: string, init: RequestInit) => {
    const auth = (init.headers as Record<string, string>).Authorization;
    const { status, body } = impl(auth);
    return {
      ok: status === 200,
      status,
      json: async () => body,
      text: async () => JSON.stringify(body),
    };
  });
  vi.stubGlobal("fetch", mock);
  return mock;
}

beforeEach(() => {
  jar.clear();
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("token sign-in", () => {
  beforeEach(() => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("KUBEHERO_DASHBOARD_AUTH", "token");
  });

  it("refuses to sign in (without calling the control plane) when production has no session secret", async () => {
    vi.stubEnv("NODE_ENV", "production");
    vi.stubEnv("KUBEHERO_SESSION_SECRET", "");
    const mock = stubWhoAmI(() => ({ status: 200, body: { subject: "key:1", role: "admin", authRequired: true } }));
    const to = await redirectOf(signInWithToken(form({ token: "good-token", next: "/logs" })));
    expect(to).toContain("error=no_session_secret");
    expect(mock).not.toHaveBeenCalled();
    expect(jar.has("kh_session")).toBe(false);
    expect(await redirectOf(signInAnonymous(form({})))).toContain("error=no_session_secret");
  });

  it("validates with WhoAmI and stores the encrypted token + role", async () => {
    const mock = stubWhoAmI((auth) =>
      auth === "Bearer good-token"
        ? { status: 200, body: { subject: "key:1a2b3c4d", role: "member", authRequired: true } }
        : { status: 401, body: { code: "unauthenticated", message: "invalid token" } },
    );
    const to = await redirectOf(signInWithToken(form({ token: "Bearer good-token", next: "/logs" })));
    expect(to).toBe("/logs");
    expect(mock.mock.calls[0][0]).toBe(`${CP}/kubehero.v1.ControlPlaneService/WhoAmI`);

    const cookie = jar.get("kh_session")!;
    expect(cookie.value).not.toContain("good-token");
    expect(cookie.options).toMatchObject({ httpOnly: true, maxAge: 43200 });
    const s = verifySession(cookie.value)!;
    expect(s).toMatchObject({ mode: "token", token: "good-token", role: "member", subject: "key:1a2b3c4d" });
    expect(s.expiresAt).toBeGreaterThan(Date.now());
  });

  it("rejects a bad token without setting a cookie", async () => {
    stubWhoAmI(() => ({ status: 401, body: { code: "unauthenticated", message: "invalid token" } }));
    const to = await redirectOf(signInWithToken(form({ token: "nope", next: "/alerts" })));
    expect(to).toBe("/login?error=invalid_token&next=%2Falerts");
    expect(jar.has("kh_session")).toBe(false);
  });

  it("refuses empty, oversized and whitespace tokens before calling out", async () => {
    const mock = stubWhoAmI(() => ({ status: 200, body: {} }));
    for (const token of ["", "a".repeat(9000), "two words"]) {
      expect(await redirectOf(signInWithToken(form({ token })))).toMatch(/error=invalid_token/);
    }
    expect(mock).not.toHaveBeenCalled();
  });

  it("reports an unreachable control plane", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("ECONNREFUSED"); }));
    expect(await redirectOf(signInWithToken(form({ token: "t" })))).toMatch(/error=unreachable/);
  });

  it("never lets next= become an open redirect", async () => {
    stubWhoAmI(() => ({ status: 200, body: { subject: "s", role: "viewer" } }));
    expect(await redirectOf(signInWithToken(form({ token: "t", next: "//evil.example" })))).toBe("/overview");
  });

  it("disables the demo email sign-in", async () => {
    expect(await redirectOf(signIn(form({ email: "a@b.io" })))).toMatch(/error=token_required/);
    expect(jar.has("kh_session")).toBe(false);
  });

  it("allows tokenless sign-in only when the control plane is open", async () => {
    stubWhoAmI((auth) =>
      auth ? { status: 401, body: { code: "unauthenticated" } } : { status: 200, body: { subject: "anonymous", role: "admin", authRequired: false } },
    );
    expect(await redirectOf(signInAnonymous(form({ next: "/network" })))).toBe("/network");
    const s = verifySession(jar.get("kh_session")!.value)!;
    expect(s).toMatchObject({ mode: "token", role: "admin", subject: "anonymous" });
    expect(s.token).toBeUndefined();
  });

  it("refuses tokenless sign-in when the control plane requires auth", async () => {
    stubWhoAmI(() => ({ status: 401, body: { code: "unauthenticated", message: "missing Authorization header" } }));
    expect(await redirectOf(signInAnonymous(form({})))).toMatch(/error=auth_required/);
    expect(jar.has("kh_session")).toBe(false);
  });
});

describe("demo sign-in", () => {
  it("works only without a control plane", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    vi.stubEnv("KUBEHERO_DASHBOARD_AUTH", "demo");
    expect(await redirectOf(signIn(form({ email: "Ada@Acme.io", next: "/fleet" })))).toBe("/fleet");
    expect(verifySession(jar.get("kh_session")!.value)).toMatchObject({ mode: "demo", email: "ada@acme.io", org: "acme" });
  });
});
