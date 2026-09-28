// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Which sign-in flow the dashboard runs, from KUBEHERO_DASHBOARD_AUTH:
//
//   token  Operators paste a control-plane credential (API key or OIDC ID
//          token). The dashboard validates it with ControlPlaneService.
//          WhoAmI and then calls the control plane AS that user — RBAC is
//          enforced by the control plane, not by trust in the dashboard.
//          This is the Helm default.
//   demo   Any email, no password: the product tour. Only honoured when
//          CONTROL_PLANE_URL is unset — a dashboard wired to real stores
//          and admin RPCs must never hand out sessions to anyone who can
//          type an email address. With a control plane configured, "demo"
//          silently becomes "token" (fail closed).
//
// Unset behaves like "demo" without a control plane and "token" with one.

export type AuthMode = "demo" | "token";

type Env = Record<string, string | undefined>;

export function authMode(env: Env = process.env): AuthMode {
  const hasControlPlane = !!env.CONTROL_PLANE_URL?.trim();
  const raw = env.KUBEHERO_DASHBOARD_AUTH?.trim().toLowerCase() ?? "";
  if (raw === "token") return "token";
  if (!hasControlPlane) return raw === "" || raw === "demo" ? "demo" : "token";
  return "token";
}

let warned = false;

/** One startup-ish warning when an explicit "demo" was overridden. */
export function warnIfDemoOverridden(env: Env = process.env): void {
  if (warned) return;
  const raw = env.KUBEHERO_DASHBOARD_AUTH?.trim().toLowerCase();
  if (raw === "demo" && env.CONTROL_PLANE_URL?.trim()) {
    warned = true;
    console.warn(
      "[auth] KUBEHERO_DASHBOARD_AUTH=demo ignored because CONTROL_PLANE_URL is set — using token sign-in",
    );
  }
}
