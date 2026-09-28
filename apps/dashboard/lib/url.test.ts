// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { flatParams, hrefWith, logqlSelector, logqlString, logsHref, param, workloadHref } from "./url";

describe("url helpers", () => {
  it("flattens and drops empties", () => {
    expect(flatParams({ a: "1", b: ["2", "3"], c: "", d: undefined })).toEqual({ a: "1", b: "2" });
  });

  it("patches params and keeps the rest", () => {
    expect(hrefWith("/logs", { q: "{a=\"b\"}", window: "1h" }, { tab: "patterns", window: null })).toBe(
      "/logs?q=%7Ba%3D%22b%22%7D&tab=patterns",
    );
    expect(hrefWith("/x", {}, {})).toBe("/x");
  });

  it("caps untrusted params", () => {
    expect(param({ q: " x ".padEnd(5000, "y") }, "q", 10)).toHaveLength(10);
  });

  it("encodes workload links", () => {
    expect(workloadHref("eks use1", "shop", "a/b")).toBe("/workloads/eks%20use1/shop/a%2Fb");
  });

  it("builds LogQL selectors with escaping", () => {
    expect(logqlString('say "hi" \\o/')).toBe('"say \\"hi\\" \\\\o/"');
    expect(logqlSelector({ namespace: "shop", workload: "payments", pod: undefined })).toBe('{namespace="shop", workload="payments"}');
    expect(logsHref('{app="x"}', { window: "6h" })).toBe("/logs?q=%7Bapp%3D%22x%22%7D&window=6h");
  });
});
