// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

"use client";

import Link from "next/link";
import { useEffect, useState } from "react";
import { AnimatePresence, motion } from "motion/react";
import {
  ArrowRight,
  CheckCircle2,
  Cloud,
  Copy,
  Layers,
  Radio,
} from "lucide-react";
import { finishOnboarding } from "@/app/onboarding/actions";

type Step = 1 | 2 | 3;

const PLANS = [
  {
    id: "helm",
    name: "Install via Helm",
    sub: "one command",
    blurb: "Everything runs inside your cluster. Air-gap capable.",
    tone: "var(--color-cool)",
  },
  {
    id: "manifests",
    name: "Install via manifests",
    sub: "kubectl apply",
    blurb: "Apply the bundled manifests directly. Same components.",
    tone: "var(--color-signal)",
  },
] as const;

export function OnboardingWizard({ email, org }: { email: string; org: string }) {
  const [step, setStep] = useState<Step>(1);
  const [plan, setPlan] = useState<"helm" | "manifests">("helm");
  const token = `khc_${btoa(`${org}:${Date.now()}`).replace(/=/g, "").slice(0, 24)}`;
  const helmCmd = `helm install kubehero kubehero/kubehero \\
  --namespace kubehero-system --create-namespace \\
  --set org=${org} \\
  --set agent.token=${token}`;

  return (
    <main className="relative flex min-h-screen items-center justify-center px-4 py-10">
      <div className="absolute inset-0 -z-10 grid-bg" aria-hidden />
      <div className="w-full max-w-[720px]">
        <div className="mb-6 flex items-center justify-between font-mono text-[11px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          <span>
            <span className="text-[var(--color-accent)]">▲</span> KubeHero · onboarding
          </span>
          <span>{email}</span>
        </div>

        <StepperHeader step={step} />

        <div className="bracketed border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] p-6 md:p-8">
          <AnimatePresence mode="wait">
            {step === 1 && (
              <motion.div
                key="plan"
                initial={{ opacity: 0, y: 6 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -6 }}
                transition={{ duration: 0.22 }}
                className="flex flex-col gap-5"
              >
                <Heading n="01" title="Choose how to run KubeHero" />
                <div className="grid gap-3 md:grid-cols-2">
                  {PLANS.map((p) => {
                    const active = plan === p.id;
                    return (
                      <button
                        key={p.id}
                        type="button"
                        onClick={() => setPlan(p.id)}
                        className={`flex flex-col gap-2 border p-4 text-left transition-colors ${
                          active
                            ? "border-[var(--color-fg)] bg-[var(--color-bg-sunken)]"
                            : "border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)]/40 hover:border-[var(--color-fg-dim)]"
                        }`}
                        style={
                          active
                            ? { borderColor: p.tone, boxShadow: `inset 0 0 0 1px ${p.tone}` }
                            : {}
                        }
                      >
                        <span className="flex items-center justify-between">
                          <span className="font-mono text-[13px] text-[var(--color-fg)]">
                            {p.name}
                          </span>
                          {active && (
                            <CheckCircle2 className="h-4 w-4" style={{ color: p.tone }} />
                          )}
                        </span>
                        <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
                          {p.sub}
                        </span>
                        <span className="mt-2 text-[12.5px] leading-snug text-[var(--color-fg-dim)]">
                          {p.blurb}
                        </span>
                      </button>
                    );
                  })}
                </div>
                <div className="mt-3 flex justify-end">
                  <button type="button" onClick={() => setStep(2)} className="btn-primary">
                    Next · connect cluster <ArrowRight className="h-3.5 w-3.5" />
                  </button>
                </div>
              </motion.div>
            )}

            {step === 2 && (
              <motion.div
                key="connect"
                initial={{ opacity: 0, y: 6 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -6 }}
                transition={{ duration: 0.22 }}
                className="flex flex-col gap-5"
              >
                <Heading n="02" title="Install the agent" />
                <p className="text-[13px] text-[var(--color-fg-dim)]">
                  Run this one command against the cluster you want to onboard.
                  The agent is read-only, ships on `helm upgrade`, and takes
                  about 15 seconds to start reporting.
                </p>
                <HelmBlock cmd={helmCmd} />
                <ConnectingSimulator onReady={() => setStep(3)} />
                <div className="flex justify-between">
                  <button
                    type="button"
                    onClick={() => setStep(1)}
                    className="btn-secondary"
                  >
                    Back
                  </button>
                  <button
                    type="button"
                    onClick={() => setStep(3)}
                    className="btn-secondary"
                  >
                    Skip — I'll connect later
                  </button>
                </div>
              </motion.div>
            )}

            {step === 3 && (
              <motion.div
                key="ready"
                initial={{ opacity: 0, y: 6 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -6 }}
                transition={{ duration: 0.22 }}
                className="flex flex-col gap-5"
              >
                <Heading n="03" title="You're set" />
                <p className="text-[13px] text-[var(--color-fg-dim)]">
                  Demo data is loaded for{" "}
                  <span className="font-mono text-[var(--color-fg)]">{org}</span>.
                  Explore the dashboard — every panel updates in real time. When
                  you&apos;re ready to connect a real cluster, the agent install
                  command is always in{" "}
                  <Link href="/settings" className="text-[var(--color-cool)]">
                    Settings
                  </Link>
                  .
                </p>
                <div className="grid gap-[1px] border border-[var(--color-line)] bg-[var(--color-line)] sm:grid-cols-3">
                  <Hint icon={Layers} title="Fleet"     body="Every cluster, rolled up by cost + state" />
                  <Hint icon={Radio}  title="Waste"     body="Top rightsizing candidates, ranked" />
                  <Hint icon={Cloud}  title="Chargeback" body="Spend by team, nodepool, cloud" />
                </div>
                <form action={finishOnboarding}>
                  <button type="submit" className="btn-primary w-full justify-center">
                    Enter the dashboard <ArrowRight className="h-3.5 w-3.5" />
                  </button>
                </form>
              </motion.div>
            )}
          </AnimatePresence>
        </div>
      </div>
    </main>
  );
}

function StepperHeader({ step }: { step: Step }) {
  const labels = ["Plan", "Connect", "Ready"];
  return (
    <div className="mb-4 grid grid-cols-3 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)]">
      {labels.map((l, i) => {
        const n = (i + 1) as Step;
        const active = n === step;
        const done = n < step;
        return (
          <div
            key={l}
            className="relative flex items-center gap-2 border-r border-[var(--color-line)] px-4 py-2.5 last:border-r-0 font-mono text-[10.5px] uppercase tracking-[0.14em]"
            style={{
              color: active ? "var(--color-fg)" : done ? "var(--color-fg-dim)" : "var(--color-fg-faint)",
            }}
          >
            <span>0{n}</span>
            <span>·</span>
            <span>{l}</span>
            {done && <CheckCircle2 className="ml-auto h-3 w-3 text-[var(--color-signal)]" />}
            {active && (
              <motion.span
                layoutId="step-underline"
                className="absolute inset-x-0 bottom-0 h-[2px] bg-[var(--color-signal)]"
              />
            )}
          </div>
        );
      })}
    </div>
  );
}

function Heading({ n, title }: { n: string; title: string }) {
  return (
    <div>
      <div className="mb-1 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        /// step {n}
      </div>
      <div className="text-[18px] font-medium text-[var(--color-fg)]">{title}</div>
    </div>
  );
}

function HelmBlock({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="relative border border-[var(--color-line-bright)] bg-[var(--color-bg-sunken)] font-mono text-[12px]">
      <pre className="overflow-x-auto whitespace-pre px-4 py-3 text-[var(--color-fg)]">
        <code>{cmd}</code>
      </pre>
      <button
        type="button"
        onClick={() => {
          navigator.clipboard?.writeText(cmd).then(() => {
            setCopied(true);
            setTimeout(() => setCopied(false), 1600);
          });
        }}
        className="absolute right-2 top-2 inline-flex items-center gap-1 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] px-1.5 py-0.5 text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
      >
        {copied ? <CheckCircle2 className="h-3 w-3 text-[var(--color-signal)]" /> : <Copy className="h-3 w-3" />}
        {copied ? "copied" : "copy"}
      </button>
    </div>
  );
}

function ConnectingSimulator({ onReady }: { onReady: () => void }) {
  const [tick, setTick] = useState(0);
  useEffect(() => {
    if (tick >= 4) return;
    const id = setTimeout(() => setTick((t) => t + 1), 1400);
    return () => clearTimeout(id);
  }, [tick]);

  const lines = [
    "agent authenticating · OIDC role",
    "mTLS handshake · cert-manager issued",
    "node discovery · 6 clusters · 502 nodes",
    "telemetry flowing · 12,480 pods attributed",
  ];

  return (
    <div className="flex flex-col gap-2 border border-[var(--color-line)] bg-[var(--color-bg-sunken)] p-4">
      {lines.map((line, i) => (
        <div
          key={i}
          className="flex items-center gap-2 font-mono text-[11.5px]"
          style={{
            opacity: i < tick ? 1 : i === tick ? 0.7 : 0.2,
            color: i < tick ? "var(--color-signal)" : "var(--color-fg-dim)",
          }}
        >
          {i < tick ? (
            <CheckCircle2 className="h-3 w-3" />
          ) : i === tick ? (
            <motion.span
              className="h-2 w-2 rounded-full bg-[var(--color-cool)]"
              animate={{ opacity: [1, 0.3, 1] }}
              transition={{ duration: 1, repeat: Infinity }}
            />
          ) : (
            <span className="h-2 w-2 rounded-full bg-[var(--color-line-bright)]" />
          )}
          <span>{line}</span>
        </div>
      ))}
      {tick >= 4 && (
        <div className="mt-2 flex items-center justify-between border-t border-[var(--color-line)] pt-2 font-mono text-[11px]">
          <span className="text-[var(--color-signal)]">
            ✓ connected · ready to explore
          </span>
          <button
            type="button"
            onClick={onReady}
            className="text-[var(--color-cool)] hover:text-[var(--color-fg)]"
          >
            continue →
          </button>
        </div>
      )}
    </div>
  );
}

function Hint({
  icon: Icon,
  title,
  body,
}: {
  icon: React.ComponentType<{ className?: string }>;
  title: string;
  body: string;
}) {
  return (
    <div className="flex flex-col gap-1.5 bg-[var(--color-bg-sunken)] px-3 py-3">
      <div className="flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        <Icon className="h-3 w-3" />
        {title}
      </div>
      <div className="text-[12px] text-[var(--color-fg-dim)]">{body}</div>
    </div>
  );
}
