// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const jar = vi.hoisted(() => new Map<string, string>());
vi.mock("next/headers", () => ({
  cookies: async () => ({
    get: (name: string) => (jar.has(name) ? { name, value: jar.get(name)! } : undefined),
    set: () => {},
  }),
}));
vi.mock("next/navigation", () => ({
  redirect: (to: string) => {
    throw Object.assign(new Error("NEXT_REDIRECT"), { to });
  },
}));

import { NextRequest } from "next/server";
import { signSession } from "@/lib/session-crypto";
import { GET } from "./route";

const CP = "http://cp.test:18080";

function req(q: string, ac = new AbortController()) {
  return { r: new NextRequest(`http://dash.test/api/logs/tail?q=${encodeURIComponent(q)}`, { signal: ac.signal }), ac };
}

async function firstEvent(res: Response): Promise<string> {
  const reader = res.body!.getReader();
  const { value } = await reader.read();
  await reader.cancel();
  return new TextDecoder().decode(value);
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

describe("GET /api/logs/tail", () => {
  it("is 401 without a session", async () => {
    const res = await GET(req('{namespace="shop"}').r);
    expect(res.status).toBe(401);
  });

  describe("signed in (token mode)", () => {
    beforeEach(() => {
      vi.stubEnv("CONTROL_PLANE_URL", CP);
      vi.stubEnv("KUBEHERO_DASHBOARD_AUTH", "token");
      jar.set("kh_session", signSession({ email: "", org: "o", onboarded: true, createdAt: 1, mode: "token", token: "user-tok", role: "viewer" }));
    });

    it("rejects metric queries and oversize input before calling upstream", async () => {
      const fetchMock = vi.fn();
      vi.stubGlobal("fetch", fetchMock);
      expect((await GET(req('sum(count_over_time({namespace="shop"}[5m]))').r)).status).toBe(400);
      expect((await GET(req(`{a="${"x".repeat(5000)}"}`).r)).status).toBe(400);
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it("streams the user's tail with their token", async () => {
      const fetchMock = vi.fn(async (_url: string, init: RequestInit) => {
        expect((init.headers as Record<string, string>).Authorization).toBe("Bearer user-tok");
        return new Response(new ReadableStream(), { status: 200, headers: { "content-type": "application/connect+json" } });
      });
      vi.stubGlobal("fetch", fetchMock);
      const { r, ac } = req('{namespace="shop"}');
      const res = await GET(r);
      expect(res.headers.get("content-type")).toContain("text/event-stream");
      expect(fetchMock.mock.calls[0][0]).toBe(`${CP}/kubehero.v1.LogsService/TailLogs`);
      ac.abort();
    });

    it("falls back to the demo tail when the control plane has no LogsService", async () => {
      vi.stubGlobal("fetch", vi.fn(async () => new Response("404 page not found\n", { status: 404 })));
      const { r, ac } = req('{namespace="shop"}');
      const res = await GET(r);
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type")).toContain("text/event-stream");
      const first = await firstEvent(res);
      expect(first).toContain("event: lines");
      expect(first).toContain('"demo":true');
      ac.abort();
    });

    it("maps other upstream failures to HTTP errors instead of demo data", async () => {
      vi.stubGlobal(
        "fetch",
        vi.fn(async () => new Response(JSON.stringify({ code: "permission_denied", message: "nope" }), { status: 403 })),
      );
      const res = await GET(req('{namespace="shop"}').r);
      expect(res.status).toBe(403);
      expect(await res.json()).toEqual({ error: { code: "permission_denied", message: "nope" } });
    });
  });
});
