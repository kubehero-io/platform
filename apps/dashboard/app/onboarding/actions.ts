// SPDX-License-Identifier: BUSL-1.1
"use server";

import { redirect } from "next/navigation";
import { setSession } from "@/lib/session";

export async function finishOnboarding() {
  await setSession({ onboarded: true });
  redirect("/fleet");
}
