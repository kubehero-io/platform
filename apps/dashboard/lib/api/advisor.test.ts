// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, describe, expect, it, vi } from "vitest";
import { getBriefing, isAdvisorLive } from "./advisor";
import { DEMO_BRIEFING } from "../advisor-demo";
import type { GetBriefingResponse } from "./types";

const ADVISOR_URL = "http://advisor.test:18081";

const RESPONSE: GetBriefingResponse = {
  briefing: {
    id: "brief-live-1",
    generatedAtUnix: 1_783_576_800,
    headline: "Fleet is healthy.",
    markdown: "## All quiet\n\nNothing to report.",
    spokenScript: "All quiet on the fleet today.",
    source: "llm",
    actions: [
      {
        id: "act-1",
        title: "Rightsize vectordb-ingress",
        impactMonthlyUsd: 8600,
        risk: "low",
        kind: "rightsize.requests",
        target: "eks-use1-prod/retrieval/vectordb-ingress",
        rationale: "p95 usage 0.41 of 16 requested cores.",
        crdYaml: "apiVersion: kubehero.kubehero.io/v1\nkind: RightsizingPolicy",
        status: "proposed",
      },
    ],
  },
};

function stubFetch(impl: (...args: Parameters<typeof fetch>) => Promise<unknown>) {
  const mock = vi.fn(impl);
  vi.stubGlobal("fetch", mock);
  return mock;
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("advisor client with ADVISOR_URL unset", () => {
  it("isAdvisorLive() is false and getBriefing() returns null without fetching", async () => {
    vi.stubEnv("ADVISOR_URL", "");
    const mock = stubFetch(async () => {
      throw new Error("should not be called");
    });
    expect(isAdvisorLive()).toBe(false);
    expect(await getBriefing()).toBeNull();
    expect(mock).not.toHaveBeenCalled();
  });
});

describe("advisor client with ADVISOR_URL set", () => {
  it("isAdvisorLive() is true", () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    expect(isAdvisorLive()).toBe(true);
  });

  it("posts a Connect JSON GetBriefing and returns the parsed response", async () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    const mock = stubFetch(async () => ({
      ok: true,
      json: async () => RESPONSE,
    }));

    const res = await getBriefing();
    expect(res).toEqual(RESPONSE);

    expect(mock).toHaveBeenCalledOnce();
    const [url, init] = mock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${ADVISOR_URL}/kubehero.v1.AdvisorService/GetBriefing`);
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
    });
    // No clusterId → the field is omitted entirely, window always sent.
    expect(JSON.parse(String(init.body))).toEqual({ window: "24h" });
  });

  it("includes clusterId in the request when given", async () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    const mock = stubFetch(async () => ({
      ok: true,
      json: async () => RESPONSE,
    }));
    await getBriefing("eks-use1-prod");
    const [, init] = mock.mock.calls[0] as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toEqual({
      clusterId: "eks-use1-prod",
      window: "24h",
    });
  });

  it("strips a trailing slash from the endpoint", async () => {
    vi.stubEnv("ADVISOR_URL", `${ADVISOR_URL}/`);
    const mock = stubFetch(async () => ({
      ok: true,
      json: async () => RESPONSE,
    }));
    await getBriefing();
    const [url] = mock.mock.calls[0] as [string];
    expect(url).toBe(`${ADVISOR_URL}/kubehero.v1.AdvisorService/GetBriefing`);
  });

  it("returns null on a non-2xx response", async () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch(async () => ({
      ok: false,
      status: 503,
      text: async () => "unavailable",
    }));
    expect(await getBriefing()).toBeNull();
  });

  it("returns null when fetch throws", async () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch(async () => {
      throw new Error("ECONNREFUSED");
    });
    expect(await getBriefing()).toBeNull();
  });

  it("returns null when the server sends an empty envelope", async () => {
    vi.stubEnv("ADVISOR_URL", ADVISOR_URL);
    stubFetch(async () => ({
      ok: true,
      json: async () => ({}),
    }));
    expect(await getBriefing()).toBeNull();
  });
});

describe("demo fixture wire-shape", () => {
  it("matches the AdvisorService contract", () => {
    const b = DEMO_BRIEFING;
    expect(b.id).toBeTruthy();
    expect(b.generatedAtUnix).toBeGreaterThan(1_700_000_000);
    expect(b.headline.length).toBeGreaterThan(20);
    expect(b.source).toBe("demo");
    expect(typeof b.markdown).toBe("string");
    expect(typeof b.spokenScript).toBe("string");
  });

  it("has a substantial markdown body (~8 paragraphs) and a ~60s script", () => {
    // Paragraph = non-empty, non-heading, non-list block separated by blanks.
    const paragraphs = DEMO_BRIEFING.markdown
      .split(/\n{2,}/)
      .filter((p) => p.trim() && !p.trim().startsWith("#") && !/^[-*\d]/.test(p.trim()));
    expect(paragraphs.length).toBeGreaterThanOrEqual(6);

    const words = DEMO_BRIEFING.spokenScript.trim().split(/\s+/).length;
    // 60s at ~165wpm·1.05 ≈ 173 words; allow a sane band.
    expect(words).toBeGreaterThanOrEqual(120);
    expect(words).toBeLessThanOrEqual(260);
  });

  it("ships 4 well-formed guarded actions", () => {
    expect(DEMO_BRIEFING.actions).toHaveLength(4);
    const risks = new Set(["low", "medium", "high"]);
    const kinds = new Set([
      "rightsize.requests",
      "ceiling.arm",
      "nodepool.consolidate",
      "workload.investigate",
    ]);
    for (const a of DEMO_BRIEFING.actions) {
      expect(a.id).toBeTruthy();
      expect(a.title).toBeTruthy();
      expect(a.impactMonthlyUsd).toBeGreaterThan(0);
      expect(risks.has(a.risk)).toBe(true);
      expect(kinds.has(a.kind)).toBe(true);
      expect(a.target.split("/")).toHaveLength(3);
      expect(a.rationale.length).toBeGreaterThan(40);
      expect(a.status).toBe("proposed");
      // CRD snippets must be real KubeHero policy kinds.
      expect(a.crdYaml).toContain("apiVersion: kubehero.kubehero.io/v1");
      expect(a.crdYaml).toMatch(/kind: (BudgetPolicy|CeilingPolicy|RightsizingPolicy)/);
    }
    // All ids unique.
    expect(new Set(DEMO_BRIEFING.actions.map((a) => a.id)).size).toBe(4);
  });

  it("only proposes guarded CRDs — humanArm or recommend-only mode", () => {
    for (const a of DEMO_BRIEFING.actions) {
      const guarded =
        a.crdYaml.includes("humanArm: true") || a.crdYaml.includes("mode: recommend");
      expect(guarded, `${a.id} must be guarded`).toBe(true);
    }
  });
});
