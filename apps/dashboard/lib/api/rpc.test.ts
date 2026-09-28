// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// Cookie jar standing in for next/headers, and a redirect() spy.
const jar = vi.hoisted(() => new Map<string, string>());
const redirectSpy = vi.hoisted(() => vi.fn((to: string) => {
  throw Object.assign(new Error("NEXT_REDIRECT"), { digest: `NEXT_REDIRECT;${to}` });
}));

vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => (jar.has(name) ? { name, value: jar.get(name)! } : undefined),
    set: () => {},
  }),
}));
vi.mock("next/navigation", () => ({ redirect: redirectSpy }));

import { signSession } from "../session-crypto";
import type { Session } from "../session";
import {
  callUnary,
  codeFromStatus,
  describeRpcError,
  parseConnectError,
  resolveCredential,
  sharedCredential,
} from "./rpc";

const CP = "http://cp.test:18080";
const ADV = "http://advisor.test:18081";

function stubFetch(impl: (...args: Parameters<typeof fetch>) => Promise<unknown>) {
  const mock = vi.fn(impl);
  vi.stubGlobal("fetch", mock);
  return mock;
}

function signIn(s: Partial<Session>) {
  jar.set(
    "kh_session",
    signSession({ email: "", org: "o", onboarded: true, createdAt: 1, ...s } as Session),
  );
}

beforeEach(() => {
  jar.clear();
  redirectSpy.mockClear();
  vi.spyOn(console, "error").mockImplementation(() => {});
});
afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("resolveCredential", () => {
  it("uses the shared CONTROL_PLANE_TOKEN without a session", async () => {
    vi.stubEnv("CONTROL_PLANE_TOKEN", "shared-cp");
    expect(await resolveCredential("cp")).toEqual({ header: "Bearer shared-cp", source: "shared" });
  });

  it("uses ADVISOR_TOKEN for the advisor", async () => {
    vi.stubEnv("ADVISOR_TOKEN", "shared-adv");
    expect(await resolveCredential("advisor")).toEqual({ header: "Bearer shared-adv", source: "shared" });
  });

  it("sends nothing when no shared token is configured", async () => {
    vi.stubEnv("CONTROL_PLANE_TOKEN", "");
    expect(await resolveCredential("cp")).toEqual({ header: null, source: "none" });
  });

  it("prefers the signed-in user's token in token mode — for both upstreams", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("KUBEHERO_DASHBOARD_AUTH", "token");
    vi.stubEnv("CONTROL_PLANE_TOKEN", "shared-cp");
    vi.stubEnv("ADVISOR_TOKEN", "shared-adv");
    signIn({ mode: "token", token: "user-tok", role: "member" });
    expect(await resolveCredential("cp")).toEqual({ header: "Bearer user-tok", source: "user" });
    expect(await resolveCredential("advisor")).toEqual({ header: "Bearer user-tok", source: "user" });
  });

  it("sends no header for an anonymous session against an open control plane", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("CONTROL_PLANE_TOKEN", "shared-cp");
    signIn({ mode: "token", role: "admin", subject: "anonymous" });
    expect(await resolveCredential("cp")).toEqual({ header: null, source: "user" });
  });

  it("ignores a demo session once the dashboard is in token mode — and never falls back to the shared key", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("CONTROL_PLANE_TOKEN", "shared-cp");
    vi.stubEnv("ADVISOR_TOKEN", "shared-adv");
    signIn({ mode: "demo", role: "admin" });
    expect(await resolveCredential("cp")).toEqual({ header: null, source: "none" });
    expect(await resolveCredential("advisor")).toEqual({ header: null, source: "none" });
  });

  it("sends nothing without a session in token mode, even with a shared key configured", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("CONTROL_PLANE_TOKEN", "shared-cp");
    expect(await resolveCredential("cp")).toEqual({ header: null, source: "none" });
    // Explicit opt-in still works for a deliberately unauthenticated call.
    expect(sharedCredential("cp")).toEqual({ header: "Bearer shared-cp", source: "shared" });
  });

  it("keeps using the shared key for demo sessions", async () => {
    vi.stubEnv("ADVISOR_URL", ADV);
    vi.stubEnv("ADVISOR_TOKEN", "shared-adv");
    signIn({ mode: "demo", role: "admin" });
    expect(await resolveCredential("advisor")).toEqual({ header: "Bearer shared-adv", source: "shared" });
  });
});

describe("callUnary", () => {
  it("logs failures unless the caller marked the code as an expected answer", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    stubFetch(async () => ({
      ok: false,
      status: 401,
      text: async () => JSON.stringify({ code: "unauthenticated", message: "missing Authorization header" }),
    }));
    const log = vi.mocked(console.error);
    const probe = { credential: { header: null, source: "none" as const }, onExpired: "return" as const };
    await callUnary("cp", "S", "WhoAmI", {}, { ...probe, expected: ["unauthenticated"] });
    expect(log).not.toHaveBeenCalled();
    await callUnary("cp", "S", "WhoAmI", {}, { ...probe, expected: ["permission_denied"] });
    expect(log).toHaveBeenCalledTimes(1);
    // The log line names method and code — never a credential or body.
    expect(log.mock.calls[0].join(" ")).toContain("WhoAmI unauthenticated");
  });

  it("returns not_configured without fetching when the URL is unset", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const mock = stubFetch(async () => {
      throw new Error("no");
    });
    const r = await callUnary("cp", "S", "M", {});
    expect(r).toEqual({ ok: false, error: { code: "not_configured", message: "CONTROL_PLANE_URL is not set" } });
    expect(mock).not.toHaveBeenCalled();
  });

  it("posts Connect JSON with a deadline and the bearer token", async () => {
    vi.stubEnv("ADVISOR_URL", `${ADV}/`);
    vi.stubEnv("ADVISOR_TOKEN", "adv-secret");
    const mock = stubFetch(async () => ({ ok: true, json: async () => ({ x: 1 }) }));
    const r = await callUnary<{ x: number }>("advisor", "kubehero.v1.AdvisorService", "ListAdvice", { a: 1 }, { timeoutMs: 1234 });
    expect(r).toEqual({ ok: true, data: { x: 1 } });
    const [url, init] = mock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADV}/kubehero.v1.AdvisorService/ListAdvice`);
    expect(init.headers).toMatchObject({
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      "Connect-Timeout-Ms": "1234",
      Authorization: "Bearer adv-secret",
    });
    expect(init.signal).toBeInstanceOf(AbortSignal);
  });

  it("maps a Connect error body", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    stubFetch(async () => ({
      ok: false,
      status: 403,
      text: async () => JSON.stringify({ code: "permission_denied", message: 'requires role "admin", caller is "viewer"' }),
    }));
    const r = await callUnary("cp", "S", "ArmPolicy", {});
    expect(r).toEqual({
      ok: false,
      error: { code: "permission_denied", message: 'requires role "admin", caller is "viewer"', status: 403 },
    });
  });

  it("maps timeouts to deadline_exceeded", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    stubFetch(async () => {
      throw Object.assign(new Error("The operation was aborted due to timeout"), { name: "TimeoutError" });
    });
    const r = await callUnary("cp", "S", "Slow", {}, { timeoutMs: 5 });
    expect(r.ok).toBe(false);
    if (!r.ok) expect(r.error.code).toBe("deadline_exceeded");
  });

  it("redirects through /api/auth/expired when the USER token is rejected", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    signIn({ mode: "token", token: "revoked", role: "admin" });
    stubFetch(async () => ({ ok: false, status: 401, text: async () => '{"code":"unauthenticated","message":"invalid token"}' }));
    await expect(callUnary("cp", "S", "M", {})).rejects.toThrow("NEXT_REDIRECT");
    expect(redirectSpy).toHaveBeenCalledWith("/api/auth/expired");
  });

  it("returns the error instead when asked to (route handlers)", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    signIn({ mode: "token", token: "revoked", role: "admin" });
    stubFetch(async () => ({ ok: false, status: 401, text: async () => '{"code":"unauthenticated"}' }));
    const r = await callUnary("cp", "S", "M", {}, { onExpired: "return" });
    expect(r.ok).toBe(false);
    expect(redirectSpy).not.toHaveBeenCalled();
  });

  it("does not redirect when a SHARED token is rejected (misconfiguration, not a user session)", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP);
    vi.stubEnv("CONTROL_PLANE_TOKEN", "wrong");
    stubFetch(async () => ({ ok: false, status: 401, text: async () => '{"code":"unauthenticated"}' }));
    const r = await callUnary("cp", "S", "M", {});
    expect(r.ok).toBe(false);
    expect(redirectSpy).not.toHaveBeenCalled();
  });
});

describe("error helpers", () => {
  it.each([
    [401, "unauthenticated"],
    [403, "permission_denied"],
    [404, "unimplemented"],
    [429, "unavailable"],
    [503, "unavailable"],
    [418, "unknown"],
  ] as const)("status %d → %s", (status, code) => {
    expect(codeFromStatus(status)).toBe(code);
  });

  it("falls back to the HTTP status for non-Connect bodies", () => {
    expect(parseConnectError(502, "<html>bad gateway</html>")).toMatchObject({ code: "unavailable", status: 502 });
    expect(parseConnectError(400, '{"code":"made_up"}')).toMatchObject({ code: "invalid_argument" });
  });

  it("describes permission errors in plain words", () => {
    expect(describeRpcError({ code: "permission_denied", message: "x" })).toMatch(/admin/);
  });
});
