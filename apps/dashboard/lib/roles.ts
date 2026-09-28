// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Role model mirrored from the control plane (internal/auth). Pure and
// client-safe: the UI uses it to hide admin-only controls, the server
// uses it to refuse admin-only server actions before the control plane
// has to. The control plane stays the authority either way.
//
//   anonymous < viewer ≈ auditor < member < admin < owner
//
// Auditor sits beside viewer: read-everywhere, never mutates.

export type Role = "anonymous" | "viewer" | "auditor" | "member" | "admin" | "owner";

const RANK: Record<Role, number> = {
  anonymous: 0,
  viewer: 1,
  auditor: 1,
  member: 2,
  admin: 3,
  owner: 4,
};

export function isRole(v: unknown): v is Role {
  return typeof v === "string" && v in RANK;
}

export function normalizeRole(v: unknown): Role {
  const s = typeof v === "string" ? v.trim().toLowerCase() : "";
  return isRole(s) ? s : "viewer";
}

export function atLeast(role: Role | undefined, min: Role): boolean {
  if (!role) return false;
  return RANK[role] >= RANK[min];
}

/** Arming policies, alert rules and silences need admin on the control plane. */
export function canAdmin(role: Role | undefined): boolean {
  return atLeast(role, "admin");
}

export function roleTone(role: Role | undefined): string {
  switch (role) {
    case "owner":
    case "admin":
      return "var(--color-accent)";
    case "member":
      return "var(--color-cool)";
    case "auditor":
    case "viewer":
      return "var(--color-fg-dim)";
    default:
      return "var(--color-fg-faint)";
  }
}
