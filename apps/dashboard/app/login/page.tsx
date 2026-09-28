// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import Link from "next/link";
import { ArrowRight, KeyRound, ShieldAlert } from "lucide-react";
import { AuthShell } from "@/components/auth/auth-shell";
import { authMode } from "@/lib/auth-mode";
import { whoAmI } from "@/lib/api/client";
import { signIn, signInAnonymous, signInWithToken } from "./actions";

export const metadata = { title: "Sign in · KubeHero" };
export const dynamic = "force-dynamic";

const ERRORS: Record<string, string> = {
  invalid_email: "That email doesn't look right.",
  invalid_token: "The control plane rejected that token.",
  unreachable: "Couldn't reach the control plane — try again in a moment.",
  no_control_plane: "Token sign-in needs CONTROL_PLANE_URL to be configured.",
  auth_required: "The control plane requires a token.",
  token_required: "This dashboard signs in with an access token.",
  wrong_mode: "That sign-in method is not enabled here.",
};

const REASONS: Record<string, string> = {
  expired: "Your session expired or the token was revoked — sign in again.",
  signed_out: "Signed out.",
};

const inputCls =
  "border border-[var(--color-line-bright)] bg-[var(--color-bg)] px-3 py-2.5 text-[14px] text-[var(--color-fg)] outline-none placeholder:text-[var(--color-fg-faint)] focus:border-[var(--color-fg-dim)] focus-visible:ring-1 focus-visible:ring-[var(--color-cool)]";

export default async function LoginPage({
  searchParams,
}: {
  searchParams: Promise<{ next?: string; error?: string; reason?: string }>;
}) {
  const { next, error, reason } = await searchParams;
  const mode = authMode();
  const nextPath = typeof next === "string" ? next : "/overview";
  const errorText = error ? ERRORS[error] : undefined;
  const reasonText = reason ? REASONS[reason] : undefined;

  if (mode === "demo") {
    return (
      <AuthShell
        title="Sign in"
        sub="Demo mode: use any work email. No control plane is connected, so every page serves labelled demo data."
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
          <input type="hidden" name="next" value={nextPath} />
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
              maxLength={254}
              placeholder="you@company.com"
              className={inputCls}
            />
          </label>
          <Notice error={errorText} reason={reasonText} />
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

  // Token mode. Probe whether the control plane runs open (dev) so we can
  // offer a tokenless sign-in; any failure just hides that option.
  const probe = await whoAmI({ credential: { header: null, source: "none" }, timeoutMs: 2_500 });
  const openControlPlane = probe.ok && probe.data.authRequired === false;

  return (
    <AuthShell
      title="Sign in"
      sub="Paste a KubeHero access token — an API key or an OIDC ID token. The control plane validates it and your role decides what you can change."
    >
      <form action={signInWithToken} className="flex flex-col gap-4">
        <input type="hidden" name="next" value={nextPath} />
        <label className="flex flex-col gap-1.5">
          <span className="flex items-center gap-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
            <KeyRound className="h-3 w-3" aria-hidden />
            Access token
          </span>
          <input
            name="token"
            type="password"
            required={!openControlPlane}
            autoFocus
            autoComplete="off"
            spellCheck={false}
            maxLength={8192}
            placeholder="kh_… or eyJhbGciOi…"
            className={`${inputCls} font-mono`}
          />
        </label>
        <Notice error={errorText} reason={reasonText} />
        <button type="submit" className="btn-primary justify-center">
          Sign in <ArrowRight className="h-3.5 w-3.5" />
        </button>
        <p className="font-mono text-[10px] leading-snug text-[var(--color-fg-faint)]">
          The token is encrypted into an httpOnly cookie (12h) and is only ever sent to the
          control plane — never to your browser&apos;s JavaScript. API keys come from the
          control plane&apos;s KUBEHERO_API_KEYS (<code>&lt;token&gt;:admin</code>,{" "}
          <code>:member</code>, <code>:viewer</code>).
        </p>
      </form>

      {openControlPlane && (
        <form action={signInAnonymous} className="mt-5 border-t border-[var(--color-line)] pt-4">
          <input type="hidden" name="next" value={nextPath} />
          <div className="mb-3 flex items-start gap-2 font-mono text-[11px] leading-snug text-[var(--color-warn)]">
            <ShieldAlert className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
            <span>
              This control plane accepts anonymous requests (no API keys or OIDC configured).
              Fine for a local cluster — never for production.
            </span>
          </div>
          <button type="submit" className="btn-secondary w-full justify-center">
            Continue without a token
          </button>
        </form>
      )}
    </AuthShell>
  );
}

function Notice({ error, reason }: { error?: string; reason?: string }) {
  if (error) {
    return (
      <span role="alert" className="font-mono text-[11px] text-[var(--color-danger)]">
        {error}
      </span>
    );
  }
  if (reason) {
    return <span className="font-mono text-[11px] text-[var(--color-warn)]">{reason}</span>;
  }
  return null;
}
