// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import {
  formatGeneratedAt,
  formatImpactUsd,
  formatSeconds,
  kindLabel,
  riskTone,
  speechSeconds,
  splitTarget,
} from "./advisor-format";

describe("formatImpactUsd", () => {
  it("uses k-notation at and above $1000/mo", () => {
    expect(formatImpactUsd(18200)).toBe("$18.2k/mo");
    expect(formatImpactUsd(1000)).toBe("$1.0k/mo");
    expect(formatImpactUsd(1234.5)).toBe("$1.2k/mo");
  });

  it("uses whole dollars below $1000/mo", () => {
    expect(formatImpactUsd(680)).toBe("$680/mo");
    expect(formatImpactUsd(680.4)).toBe("$680/mo");
  });

  it("clamps zero / negative / non-finite to $0", () => {
    expect(formatImpactUsd(0)).toBe("$0/mo");
    expect(formatImpactUsd(-5)).toBe("$0/mo");
    expect(formatImpactUsd(Number.NaN)).toBe("$0/mo");
  });
});

describe("formatGeneratedAt", () => {
  it("renders unix seconds as a stable UTC timestamp", () => {
    // 2026-07-09 06:00:00 UTC
    expect(formatGeneratedAt(1_783_576_800)).toBe("2026-07-09 06:00 utc");
  });

  it("pads single-digit fields", () => {
    // 2026-01-02 03:04:00 UTC = 1767225600 + 86400 + 3*3600 + 4*60
    expect(formatGeneratedAt(1_767_323_040)).toBe("2026-01-02 03:04 utc");
  });

  it("degrades to a dash on bad input", () => {
    expect(formatGeneratedAt(0)).toBe("—");
    expect(formatGeneratedAt(Number.NaN)).toBe("—");
  });
});

describe("speechSeconds", () => {
  it("estimates ~60s for a 60-second script at rate 1.05", () => {
    const s = speechSeconds(new Array(173).fill("word").join(" "), 1.05);
    expect(s).toBeGreaterThanOrEqual(55);
    expect(s).toBeLessThanOrEqual(65);
  });

  it("scales inversely with rate", () => {
    const script = new Array(200).fill("w").join(" ");
    expect(speechSeconds(script, 2)).toBeLessThan(speechSeconds(script, 1));
  });

  it("is 0 for an empty script and never below 1s otherwise", () => {
    expect(speechSeconds("")).toBe(0);
    expect(speechSeconds("hi")).toBe(1);
  });
});

describe("formatSeconds", () => {
  it("formats under a minute as raw seconds", () => {
    expect(formatSeconds(48)).toBe("48s");
  });
  it("formats minutes with zero-padded seconds", () => {
    expect(formatSeconds(64)).toBe("1m 04s");
    expect(formatSeconds(120)).toBe("2m 00s");
  });
  it("clamps negatives to 0", () => {
    expect(formatSeconds(-3)).toBe("0s");
  });
});

describe("riskTone / kindLabel", () => {
  it("maps every risk to a css var", () => {
    expect(riskTone("low")).toBe("var(--color-signal)");
    expect(riskTone("medium")).toBe("var(--color-warn)");
    expect(riskTone("high")).toBe("var(--color-accent)");
  });

  it("maps every action kind to a short label", () => {
    expect(kindLabel("rightsize.requests")).toBe("rightsize");
    expect(kindLabel("ceiling.arm")).toBe("arm ceiling");
    expect(kindLabel("nodepool.consolidate")).toBe("consolidate");
    expect(kindLabel("workload.investigate")).toBe("investigate");
  });
});

describe("splitTarget", () => {
  it("splits cluster/ns/workload", () => {
    expect(splitTarget("eks-use1-prod/retrieval/vectordb-ingress")).toEqual({
      cluster: "eks-use1-prod",
      namespace: "retrieval",
      workload: "vectordb-ingress",
    });
  });

  it("tolerates short targets", () => {
    expect(splitTarget("just-a-cluster")).toEqual({
      cluster: "just-a-cluster",
      namespace: "",
      workload: "",
    });
  });
});
