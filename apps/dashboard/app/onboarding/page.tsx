// SPDX-License-Identifier: BUSL-1.1
import { getSession } from "@/lib/session";
import { OnboardingWizard } from "@/components/onboarding-wizard";

export const metadata = { title: "Welcome · KubeHero" };

export default async function OnboardingPage() {
  const s = await getSession();
  return <OnboardingWizard email={s?.email ?? ""} org={s?.org ?? ""} />;
}
