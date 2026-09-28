// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

"use client";

// Voice briefing via the browser's built-in SpeechSynthesis API.
// Zero external TTS deps — if the browser can't speak, we degrade to a
// quiet "not supported" chip instead of hiding the feature.

import { useCallback, useEffect, useRef, useState, useSyncExternalStore } from "react";
import { AudioLines, Pause, Play, Square, VolumeX } from "lucide-react";
import { formatSeconds, speechSeconds } from "@/lib/advisor-format";

const RATE = 1.05;

const noopSubscribe = () => () => {};

type Status = "idle" | "playing" | "paused";

/** Prefer a natural en-US voice; fall back to any English, then default. */
function pickVoice(voices: SpeechSynthesisVoice[]): SpeechSynthesisVoice | null {
  if (voices.length === 0) return null;
  const preferred = ["Samantha", "Google US English", "Microsoft Aria", "Daniel", "Karen"];
  for (const name of preferred) {
    const v = voices.find((v) => v.name.includes(name));
    if (v) return v;
  }
  return (
    voices.find((v) => v.lang === "en-US" && v.localService) ??
    voices.find((v) => v.lang.startsWith("en")) ??
    voices[0]
  );
}

export function PlayBriefing({ script }: { script: string }) {
  // null during SSR, then true/false once the browser can be probed.
  const supported = useSyncExternalStore<boolean | null>(
    noopSubscribe,
    () => "speechSynthesis" in window && typeof window.SpeechSynthesisUtterance !== "undefined",
    () => null,
  );
  const [status, setStatus] = useState<Status>("idle");
  const utteranceRef = useRef<SpeechSynthesisUtterance | null>(null);

  useEffect(() => {
    return () => {
      // Never leave a voice talking after navigation.
      if (typeof window !== "undefined" && "speechSynthesis" in window) {
        window.speechSynthesis.cancel();
      }
    };
  }, []);

  const play = useCallback(() => {
    const synth = window.speechSynthesis;
    if (status === "paused") {
      synth.resume();
      setStatus("playing");
      return;
    }
    synth.cancel(); // drop anything stale
    const u = new SpeechSynthesisUtterance(script);
    u.rate = RATE;
    u.pitch = 1.0;
    const voice = pickVoice(synth.getVoices());
    if (voice) {
      u.voice = voice;
      u.lang = voice.lang;
    }
    u.onend = () => setStatus("idle");
    u.onerror = () => setStatus("idle");
    utteranceRef.current = u; // keep a ref so GC can't silence it mid-speech
    synth.speak(u);
    setStatus("playing");
  }, [script, status]);

  const pause = useCallback(() => {
    window.speechSynthesis.pause();
    setStatus("paused");
  }, []);

  const stop = useCallback(() => {
    window.speechSynthesis.cancel();
    setStatus("idle");
  }, []);

  const estimate = formatSeconds(speechSeconds(script, RATE));

  if (supported === false) {
    return (
      <span
        className="inline-flex items-center gap-1.5 border border-[var(--color-line)] px-2.5 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]"
        title="This browser does not expose the SpeechSynthesis API"
      >
        <VolumeX className="h-3 w-3" />
        voice unavailable
      </span>
    );
  }

  return (
    <div className="inline-flex items-center gap-[1px] border border-[var(--color-line-bright)] bg-[var(--color-line)]">
      {status === "playing" ? (
        <button
          type="button"
          onClick={pause}
          className="inline-flex items-center gap-2 bg-[var(--color-bg-raised)] px-3 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg)] transition-colors hover:bg-[var(--color-bg-sunken)]"
        >
          <Pause className="h-3 w-3" />
          pause
        </button>
      ) : (
        <button
          type="button"
          onClick={play}
          disabled={supported === null}
          className="inline-flex items-center gap-2 bg-[var(--color-bg-raised)] px-3 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-signal)] transition-colors hover:bg-[var(--color-bg-sunken)] disabled:opacity-50"
        >
          <Play className="h-3 w-3" />
          {status === "paused" ? "resume" : "play briefing"}
        </button>
      )}
      {status !== "idle" && (
        <button
          type="button"
          onClick={stop}
          className="inline-flex items-center gap-2 bg-[var(--color-bg-raised)] px-3 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-dim)] transition-colors hover:bg-[var(--color-bg-sunken)] hover:text-[var(--color-fg)]"
        >
          <Square className="h-3 w-3" />
          stop
        </button>
      )}
      <span className="inline-flex items-center gap-1.5 bg-[var(--color-bg-raised)] px-3 py-1.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
        {status === "playing" ? (
          <AudioLines className="h-3 w-3 text-[var(--color-signal)]" />
        ) : (
          <AudioLines className="h-3 w-3" />
        )}
        ~{estimate}
      </span>
    </div>
  );
}
