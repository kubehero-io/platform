// SPDX-License-Identifier: BUSL-1.1
"use server";

import { redirect } from "next/navigation";
import { safeNextPath } from "@/lib/auth-gate";
import { orgFromEmail, setSession } from "@/lib/session";

export async function signIn(formData: FormData) {
  const email = String(formData.get("email") || "").trim().toLowerCase();
  // Only same-origin paths — `next` comes from the query string, so an
  // unchecked value would make /login an open redirect.
  const next = safeNextPath(String(formData.get("next") || "/overview"));
  if (!email || !email.includes("@")) {
    redirect(`/login?error=invalid_email&next=${encodeURIComponent(next)}`);
  }
  await setSession({
    email,
    org: orgFromEmail(email),
    onboarded: true, // existing account — skip onboarding
    createdAt: Date.now(),
  });
  redirect(next);
}

export async function signUp(formData: FormData) {
  const email = String(formData.get("email") || "").trim().toLowerCase();
  if (!email || !email.includes("@")) {
    redirect(`/signup?error=invalid_email`);
  }
  await setSession({
    email,
    org: orgFromEmail(email),
    onboarded: false, // new account — force onboarding
    createdAt: Date.now(),
  });
  redirect("/onboarding");
}
