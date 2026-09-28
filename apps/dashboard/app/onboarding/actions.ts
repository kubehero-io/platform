// SPDX-License-Identifier: BUSL-1.1
"use server";

import { redirect } from "next/navigation";
import { authMode } from "@/lib/auth-mode";
import { getSession, setSession } from "@/lib/session";

export async function finishOnboarding() {
  // The wizard is part of the demo tour only; token sessions start onboarded.
  if (authMode() !== "demo" || !(await getSession())) redirect("/login");
  await setSession({ onboarded: true });
  redirect("/overview");
}
