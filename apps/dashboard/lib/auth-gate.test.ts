// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { gate, safeNextPath } from "./auth-gate";
import type { Session } from "./session";

const onboarded: Session = { email: "a@b.io", org: "b", onboarded: true, createdAt: 1 };
const fresh: Session = { ...onboarded, onboarded: false };

// verify() stand-in: "good" and "new" are valid cookies, anything else is forged.
const verify = (raw: string): Session | null =>
  raw === "good" ? onboarded : raw === "new" ? fresh : null;

describe("gate", () => {
  it.each([
    ["/overview", undefined, { kind: "redirect", to: "/login?next=%2Foverview", clearCookie: false }],
    ["/logs", undefined, { kind: "redirect", to: "/login?next=%2Flogs", clearCookie: false }],
    ["/", undefined, { kind: "redirect", to: "/login", clearCookie: false }],
    // forged cookie → treated as logged out and cleared
    ["/alerts", "forged", { kind: "redirect", to: "/login?next=%2Falerts", clearCookie: true }],
    ["/login", undefined, { kind: "next" }],
    ["/login", "forged", { kind: "next" }],
    ["/login", "good", { kind: "redirect", to: "/overview" }],
    ["/logs", "good", { kind: "next" }],
    ["/workloads/a/b/c", "good", { kind: "next" }],
    ["/logs", "new", { kind: "redirect", to: "/onboarding" }],
    ["/onboarding", "new", { kind: "next" }],
  ] as const)("%s with cookie %s", (path, cookie, want) => {
    expect(gate(path, cookie, verify)).toEqual(want);
  });
});

describe("safeNextPath", () => {
  it.each([
    ["/logs?q=%7B%7D", "/logs?q=%7B%7D"],
    ["//evil.example", "/overview"],
    ["https://evil.example", "/overview"],
    ["/\\evil.example", "/overview"],
    ["", "/overview"],
    [null, "/overview"],
    ["/" + "a".repeat(600), "/overview"],
  ])("%s → %s", (input, want) => {
    expect(safeNextPath(input)).toBe(want);
  });
});
