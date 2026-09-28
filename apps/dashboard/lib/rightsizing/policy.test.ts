// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { policyName, rightsizingPolicyYaml, yamlScalar, type PolicyInput } from "./policy";

const INPUT: PolicyInput = {
  cluster: "eks-use1-prod",
  namespace: "retrieval",
  workload: "vectordb-ingress",
  container: "ingress",
  window: "7d",
  headroomPct: 15,
  replicas: 4,
  siblings: ["retrieval-indexer", "vectordb-ingress", "retrieval-indexer"],
  cpuRecommended: "470m",
  memRecommended: "4.8 GiB",
  savingsUsdMonth: 8612.4,
};

describe("policyName", () => {
  it("is DNS-1123 and ≤ 63 chars", () => {
    expect(policyName("retrieval", "vectordb-ingress")).toBe("rightsize-retrieval-vectordb-ingress");
    const long = policyName("a".repeat(40), "Some_Weird.Name/x".repeat(3));
    expect(long.length).toBeLessThanOrEqual(63);
    expect(long).toMatch(/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/);
  });
});

describe("yamlScalar", () => {
  it.each([
    ["retrieval", "retrieval"],
    ["7d", "7d"],
    ["true", '"true"'],
    ["123", '"123"'],
    ["a: b", '"a: b"'],
    ['say "hi"', '"say \\"hi\\""'],
    ["", '""'],
  ])("%s → %s", (v, want) => {
    expect(yamlScalar(v)).toBe(want);
  });
});

describe("rightsizingPolicyYaml", () => {
  it("renders a guarded apply-mode policy scoped to one workload", () => {
    const y = rightsizingPolicyYaml(INPUT, "apply");
    expect(y).toContain("kind: RightsizingPolicy");
    expect(y).toContain("  name: rightsize-retrieval-vectordb-ingress");
    expect(y).toContain("        kubernetes.io/metadata.name: retrieval");
    expect(y).toContain("  mode: apply");
    expect(y).toContain("kubehero.kubehero.io/armed=true");
    // siblings de-duplicated, target itself never excluded
    expect(y.match(/- retrieval-indexer/g)).toHaveLength(1);
    expect(y).not.toContain("- vectordb-ingress");
    expect(y).toContain("    minReplicas: 2");
    expect(y).toContain("saves ~$8,612/mo");
  });

  it("drops the arming instructions for recommend mode and omits empty exclude", () => {
    const y = rightsizingPolicyYaml({ ...INPUT, siblings: [], replicas: 1 }, "recommend");
    expect(y).toContain("  mode: recommend");
    expect(y).not.toContain("armed=true");
    expect(y).not.toContain("exclude:");
    expect(y).not.toContain("minReplicas");
  });

  it("produces only spec keys the CRD knows", () => {
    const y = rightsizingPolicyYaml(INPUT);
    const specKeys = y
      .split("\n")
      .filter((l) => /^ {2}[a-zA-Z]+:/.test(l))
      .map((l) => l.trim().split(":")[0]);
    for (const k of specKeys) expect(["name", "namespace", "labels", "scope", "mode", "exclude", "safety"]).toContain(k);
  });
});
