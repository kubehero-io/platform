// SPDX-License-Identifier: BUSL-1.1
import Link from "next/link";
import { ArrowRight } from "lucide-react";
import { AuthShell } from "@/components/auth/auth-shell";
import { signIn } from "./actions";

export const metadata = { title: "Sign in · KubeHero" };

export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ next?: string; error?: string }>;
}) {
  const { next, error } = await searchParams;
  return (
    <AuthShell
      title="Sign in"
      sub="Use your work email. No password needed in demo mode."
      footer={
        <span className="font-mono text-[11px] text-[var(--color-fg-dim)]">
          No account?{" "}
          <Link href="/signup" className="text-[var(--color-cool)] hover:text-[var(--color-fg)]">
            Create one
          </Link>
        </span>
      }
    >
      <form action={signIn} className="flex flex-col gap-4">
        <input type="hidden" name="next" value={next ?? "/fleet"} />
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
          Continue <ArrowRight className="h-3.5 w-3.5" />
        </button>
        <p className="font-mono text-[10px] leading-snug text-[var(--color-fg-faint)]">
          demo mode · no password · session expires in 7d
        </p>
      </form>
    </AuthShell>
  );
}
