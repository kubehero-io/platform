// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { GET } from "./route";

describe("GET /api/auth/expired", () => {
  it("clears the session and redirects with a host-independent Location", () => {
    const res = GET();
    expect(res.status).toBe(307);
    // Relative, so an ingress-fronted dashboard never redirects to the
    // pod's internal address.
    expect(res.headers.get("location")).toBe("/login?reason=expired");
    const cookie = res.headers.get("set-cookie") ?? "";
    expect(cookie).toMatch(/^kh_session=;/);
    expect(cookie).toMatch(/Max-Age=0/i);
  });
});
