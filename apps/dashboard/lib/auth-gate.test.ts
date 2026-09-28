// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { describe, expect, it } from "vitest";
import { authRoutes, gate, safeNextPath, sessionAcceptable } from "./auth-gate";
import type { Session } from "./session";

const onboarded: Session = { email: "a@b.io", org: "b", onboarded: true, createdAt: 1 };
const fresh: Session = { ...onboarded, onboarded: false };

// verify() stand-in: "good" and "new" are valid cookies, anything else is forged.
const verify = (raw: string): Session | null =>
  raw === "good" ? onboarded : raw === "new" ? fresh : null;

describe("gate · demo mode", () => {
  it.each([
    ["/overview", undefined, { kind: "redirect", to: "/login?next=%2Foverview", clearCookie: false }],
    ["/logs", undefined, { kind: "redirect", to: "/login?next=%2Flogs", clearCookie: false }],
    ["/", undefined, { kind: "redirect", to: "/login", clearCookie: false }],
    // forged cookie → treated as logged out and cleared
    ["/alerts", "forged", { kind: "redirect", to: "/login?next=%2Falerts", clearCookie: true }],
    ["/login", undefined, { kind: "next" }],
    ["/login", "forged", { kind: "next", clearCookie: true }],
    ["/signup", undefined, { kind: "next" }],
    ["/login", "good", { kind: "redirect", to: "/overview" }],
    ["/logs", "good", { kind: "next" }],
    ["/workloads/a/b/c", "good", { kind: "next" }],
    ["/logs", "new", { kind: "redirect", to: "/onboarding" }],
    ["/onboarding", "new", { kind: "next" }],
  ] as const)("%s with cookie %s", (path, cookie, want) => {
    expect(gate(path, cookie, verify, "demo")).toEqual(want);
  });
});

describe("gate · token mode", () => {
  const tokenSession: Session = { ...onboarded, mode: "token", token: "t", role: "admin", expiresAt: 2_000 };
  const verifyTok = (raw: string): Session | null =>
    raw === "tok" ? tokenSession : raw === "good" ? onboarded : null;

  it("accepts a token session before expiry", () => {
    expect(gate("/logs", "tok", verifyTok, "token", 1_000)).toEqual({ kind: "next" });
  });
  it("refuses and clears an expired token session", () => {
    expect(gate("/logs", "tok", verifyTok, "token", 3_000)).toEqual({
      kind: "redirect", to: "/login?next=%2Flogs", clearCookie: true,
    });
  });
  it("refuses a demo session once the dashboard runs in token mode", () => {
    expect(gate("/logs", "good", verifyTok, "token", 1_000)).toEqual({
      kind: "redirect", to: "/login?next=%2Flogs", clearCookie: true,
    });
  });
  it("has no sign-up tour or onboarding", () => {
    expect(gate("/signup", undefined, verifyTok, "token")).toEqual({ kind: "redirect", to: "/login", clearCookie: false });
    expect(gate("/onboarding", "tok", verifyTok, "token", 1_000)).toEqual({ kind: "redirect", to: "/overview", clearCookie: false });
    expect(authRoutes("token").has("/signup")).toBe(false);
  });
});

describe("sessionAcceptable", () => {
  it("treats a session without a mode as a demo session", () => {
    expect(sessionAcceptable(onboarded, "demo")).toBe(true);
    expect(sessionAcceptable(onboarded, "token")).toBe(false);
  });
  it("honours expiresAt", () => {
    const s: Session = { ...onboarded, expiresAt: 10 };
    expect(sessionAcceptable(s, "demo", 9)).toBe(true);
    expect(sessionAcceptable(s, "demo", 10)).toBe(false);
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
