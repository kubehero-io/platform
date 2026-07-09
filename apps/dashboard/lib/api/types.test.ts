// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { dtoToDisplay, type Cloud, type ClusterDTO } from "./types";

const dto = (cloud: Cloud): ClusterDTO => ({
  id: "c-1",
  name: "c-1",
  cloud,
  region: "us-east-1",
  nodes: 10,
});

describe("dtoToDisplay", () => {
  it("maps proto cloud names to display names", () => {
    expect(dtoToDisplay(dto("aws")).cloud).toBe("EKS");
    expect(dtoToDisplay(dto("gcp")).cloud).toBe("GKE");
    expect(dtoToDisplay(dto("azure")).cloud).toBe("AKS");
  });

  it("defaults an unknown cloud to EKS", () => {
    expect(dtoToDisplay(dto("oci" as Cloud)).cloud).toBe("EKS");
  });

  it("uses placeholders when no fallback is given", () => {
    expect(dtoToDisplay(dto("aws"))).toMatchObject({
      costDay: "—",
      recoverable: "—",
      gpu: "—",
      state: "healthy",
    });
  });

  it("merges narrative fields from the fallback, keeping live identity fields", () => {
    const out = dtoToDisplay(dto("aws"), {
      costDay: "$1,000",
      recoverable: "$200",
      gpu: "8× A100",
      state: "warn",
      nodes: 999, // must NOT override the live value
    });
    expect(out).toMatchObject({
      id: "c-1",
      nodes: 10,
      costDay: "$1,000",
      recoverable: "$200",
      gpu: "8× A100",
      state: "warn",
    });
  });
});
