// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { authMode } from "./auth-mode";

describe("authMode", () => {
  it.each([
    [{}, "demo"],
    [{ KUBEHERO_DASHBOARD_AUTH: "demo" }, "demo"],
    [{ KUBEHERO_DASHBOARD_AUTH: "token" }, "token"],
    [{ KUBEHERO_DASHBOARD_AUTH: " TOKEN " }, "token"],
    // A real control plane is never fronted by the any-email tour.
    [{ CONTROL_PLANE_URL: "http://cp:8080" }, "token"],
    [{ CONTROL_PLANE_URL: "http://cp:8080", KUBEHERO_DASHBOARD_AUTH: "demo" }, "token"],
    [{ CONTROL_PLANE_URL: "  ", KUBEHERO_DASHBOARD_AUTH: "demo" }, "demo"],
    // Typos fail closed.
    [{ KUBEHERO_DASHBOARD_AUTH: "demoo" }, "token"],
  ] as const)("%j → %s", (env, want) => {
    expect(authMode(env)).toBe(want);
  });
});
