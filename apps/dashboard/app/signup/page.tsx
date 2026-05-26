// SPDX-License-Identifier: BUSL-1.1
import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { AuthShell } from "@/components/auth/auth-shell";
import { signUp } from "../login/actions";

export const metadata = { title: "Sign in · KubeHero" };

export default async function SignupPage({
  searchParams,
}: {
  searchParams: Promise<{ error?: string }>;
}) {
  const { error } = await searchParams;
  return (
    <AuthShell
      title="Sign in"
      sub="Spin up a demo account. You'll walk through a 3-step onboarding, then the dashboard is yours to explore."
      footer={
        <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
          Already have an account?{" "}
          <Link href="/login" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
            Sign in
          </Link>
        </span>
      }
    >
      <form action={signUp} className="flex flex-col gap-4">
        <label className="flex flex-col gap-1.5">
          <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            Work email
          </span>
          <input
            name="email"
            type="email"
            required
            autoFocus
            autoComplete="email"
            placeholder="you@company.com"
            className="border border-[var(--color-line-bright)] bg-[var(--color-bg)] px-3 py-2.5 text-[14px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] focus:border-[var(--color-fg-dim)]"
          />
        </label>
        {error === "invalid_email" && (
          <span className="font-mono text-[11px] text-[var(--color-danger)]">
            That email doesn&apos;t look right.
          </span>
        )}
        <button type="submit" className="btn-primary justify-center">
          Create account <ArrowRight className="h-3.5 w-3.5" />
        </button>
        <p className="font-mono text-[10px] leading-snug text-[var(--color-fg-faint)]">
          demo mode · no email verification · everything runs locally
        </p>
      </form>
    </AuthShell>
  );
}
