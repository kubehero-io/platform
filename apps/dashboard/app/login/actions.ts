// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use server";

// Sign-in server actions. Which ones work depends on lib/auth-mode.ts:
//   demo  → signIn / signUp (any email, no password; no control plane)
//   token → signInWithToken (credential validated by WhoAmI), or
//           signInAnonymous when the control plane itself reports that it
//           accepts anonymous callers (dev / kind clusters).

import { redirect } from "next/navigation";
import { safeNextPath } from "@/lib/auth-gate";
import { authMode, warnIfDemoOverridden } from "@/lib/auth-mode";
import { whoAmI } from "@/lib/api/client";
import { normalizeRole } from "@/lib/roles";
import { orgFromEmail, setSession, TOKEN_MAX_AGE } from "@/lib/session";

/** API keys are ~32–64 chars, OIDC ID tokens a few KiB; anything bigger is junk. */
const MAX_TOKEN_CHARS = 8192;

function loginUrl(error: string, next: string): string {
  return `/login?error=${error}&next=${encodeURIComponent(next)}`;
}

export async function signIn(formData: FormData) {
  // Only same-origin paths — `next` comes from the query string, so an
  // unchecked value would make /login an open redirect.
  const next = safeNextPath(String(formData.get("next") || "/overview"));
  warnIfDemoOverridden();
  if (authMode() !== "demo") redirect(loginUrl("token_required", next));

  const email = String(formData.get("email") || "").trim().toLowerCase().slice(0, 254);
  if (!email || !email.includes("@")) {
    redirect(`/login?error=invalid_email&next=${encodeURIComponent(next)}`);
  }
  await setSession({
    email,
    org: orgFromEmail(email),
    onboarded: true, // existing account — skip onboarding
    createdAt: Date.now(),
    mode: "demo",
    role: "admin", // the demo user may click everything; nothing persists
  });
  redirect(next);
}

export async function signUp(formData: FormData) {
  if (authMode() !== "demo") redirect("/login");
  const email = String(formData.get("email") || "").trim().toLowerCase().slice(0, 254);
  if (!email || !email.includes("@")) {
    redirect(`/signup?error=invalid_email`);
  }
  await setSession({
    email,
    org: orgFromEmail(email),
    onboarded: false, // new account — force onboarding
    createdAt: Date.now(),
    mode: "demo",
    role: "admin",
  });
  redirect("/onboarding");
}

export async function signInWithToken(formData: FormData) {
  const next = safeNextPath(String(formData.get("next") || "/overview"));
  if (authMode() !== "token") redirect(loginUrl("wrong_mode", next));

  let token = String(formData.get("token") || "").trim();
  if (/^bearer\s+/i.test(token)) token = token.replace(/^bearer\s+/i, "").trim();
  if (!token || token.length > MAX_TOKEN_CHARS || /\s/.test(token)) {
    redirect(loginUrl("invalid_token", next));
  }

  const res = await whoAmI({ credential: { header: `Bearer ${token}`, source: "none" } });
  if (!res.ok) {
    if (res.error.code === "unauthenticated" || res.error.code === "permission_denied") {
      // Slow the loop down a little; tokens are unguessable anyway.
      await new Promise((r) => setTimeout(r, 400));
      redirect(loginUrl("invalid_token", next));
    }
    redirect(loginUrl(res.error.code === "not_configured" ? "no_control_plane" : "unreachable", next));
  }

  const who = res.data;
  const role = normalizeRole(who.role);
  const email = (who.email ?? "").slice(0, 254);
  await setSession(
    {
      email,
      org: email ? orgFromEmail(email) : "kubehero",
      onboarded: true,
      createdAt: Date.now(),
      mode: "token",
      token,
      role,
      subject: (who.subject ?? "").slice(0, 200),
      expiresAt: Date.now() + TOKEN_MAX_AGE * 1000,
    },
    { maxAgeSec: TOKEN_MAX_AGE },
  );
  redirect(next);
}

/**
 * Tokenless sign-in, allowed only while the control plane itself says it
 * accepts anonymous callers (KUBEHERO_API_KEYS + OIDC unset — a dev
 * configuration the control plane warns about at startup). Re-probed on
 * every attempt so it stops working the moment auth is turned on.
 */
export async function signInAnonymous(formData: FormData) {
  const next = safeNextPath(String(formData.get("next") || "/overview"));
  if (authMode() !== "token") redirect(loginUrl("wrong_mode", next));

  const res = await whoAmI({ credential: { header: null, source: "none" } });
  if (!res.ok) {
    redirect(loginUrl(res.error.code === "unauthenticated" ? "auth_required" : "unreachable", next));
  }
  if (res.data.authRequired !== false) redirect(loginUrl("auth_required", next));

  await setSession(
    {
      email: "",
      org: "kubehero",
      onboarded: true,
      createdAt: Date.now(),
      mode: "token",
      token: undefined,
      role: normalizeRole(res.data.role),
      subject: "anonymous",
      expiresAt: Date.now() + TOKEN_MAX_AGE * 1000,
    },
    { maxAgeSec: TOKEN_MAX_AGE },
  );
  redirect(next);
}
