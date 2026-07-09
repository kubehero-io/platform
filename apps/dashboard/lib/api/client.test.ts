// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { afterEach, describe, expect, it, vi } from "vitest";
import { healthCheck, isLive, listClusters } from "./client";
import type { ListClustersResponse } from "./types";

const CP_URL = "http://cp.test:18080";

const LIST_RESPONSE: ListClustersResponse = {
  clusters: [
    { id: "eks-use1-prod", name: "eks-use1-prod", cloud: "aws", region: "us-east-1", nodes: 210 },
  ],
  nextPageToken: "",
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

describe("client with CONTROL_PLANE_URL unset", () => {
  it("isLive() is false and rpcs return null without fetching", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", "");
    const mock = stubFetch(async () => {
      throw new Error("should not be called");
    });
    expect(isLive()).toBe(false);
    expect(await listClusters()).toBeNull();
    expect(await healthCheck()).toBeNull();
    expect(mock).not.toHaveBeenCalled();
  });
});

describe("client with CONTROL_PLANE_URL set", () => {
  it("isLive() is true", () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP_URL);
    expect(isLive()).toBe(true);
  });

  it("posts a Connect JSON request and returns the parsed response", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP_URL);
    const mock = stubFetch(async () => ({
      ok: true,
      json: async () => LIST_RESPONSE,
    }));

    const res = await listClusters(25);
    expect(res).toEqual(LIST_RESPONSE);

    expect(mock).toHaveBeenCalledOnce();
    const [url, init] = mock.mock.calls[0] as [string, RequestInit];
    expect(url).toBe(`${CP_URL}/kubehero.v1.ControlPlaneService/ListClusters`);
    expect(init.method).toBe("POST");
    expect(init.headers).toMatchObject({
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
    });
    expect(JSON.parse(String(init.body))).toEqual({ pageSize: 25 });
  });

  it("strips a trailing slash from the endpoint", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", `${CP_URL}/`);
    const mock = stubFetch(async () => ({
      ok: true,
      json: async () => LIST_RESPONSE,
    }));
    await listClusters();
    const [url] = mock.mock.calls[0] as [string];
    expect(url).toBe(`${CP_URL}/kubehero.v1.ControlPlaneService/ListClusters`);
  });

  it("returns null on a non-2xx response", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP_URL);
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch(async () => ({
      ok: false,
      status: 503,
      text: async () => "unavailable",
    }));
    expect(await listClusters()).toBeNull();
  });

  it("returns null when fetch throws", async () => {
    vi.stubEnv("CONTROL_PLANE_URL", CP_URL);
    vi.spyOn(console, "error").mockImplementation(() => {});
    stubFetch(async () => {
      throw new Error("ECONNREFUSED");
    });
    expect(await listClusters()).toBeNull();
  });
});
