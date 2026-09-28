// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors
"use client";

// Live tail over SSE (/api/logs/tail, which proxies LogsService.TailLogs).
// Pause keeps receiving into a side buffer so nothing is lost on resume;
// the visible buffer is capped (oldest dropped) so a firehose can't eat
// the tab's memory.

import { useCallback, useEffect, useRef, useState } from "react";
import { ArrowDownToLine, Pause, Play, RotateCw, Trash2 } from "lucide-react";
import { LogList } from "./log-list";
import type { LogLine } from "@/lib/logs/types";
import { readSse } from "@/lib/sse";

const MAX_BUFFER = 2000;

type Status = "connecting" | "live" | "error" | "ended";

export function LiveTail({ query, height = 560 }: { query: string; height?: number }) {
  const [lines, setLines] = useState<LogLine[]>([]);
  const [paused, setPaused] = useState(false);
  const [follow, setFollow] = useState(true);
  const [status, setStatus] = useState<Status>("connecting");
  const [error, setError] = useState<string | null>(null);
  const [dropped, setDropped] = useState(0);
  const [rate, setRate] = useState(0);
  const [attempt, setAttempt] = useState(0);
  const pending = useRef<LogLine[]>([]);
  const pausedRef = useRef(false);
  const recent = useRef<{ t: number; n: number }[]>([]);

  const append = useCallback((incoming: LogLine[]) => {
    setLines((prev) => {
      const next = prev.concat(incoming);
      return next.length > MAX_BUFFER ? next.slice(next.length - MAX_BUFFER) : next;
    });
  }, []);

  useEffect(() => {
    const ctl = new AbortController();
    let reconnect: ReturnType<typeof setTimeout> | undefined;
    (async () => {
      setStatus("connecting");
      setError(null);
      try {
        const res = await fetch(`/api/logs/tail?${new URLSearchParams({ q: query }).toString()}`, {
          signal: ctl.signal,
          headers: { Accept: "text/event-stream" },
        });
        if (!res.ok) {
          const body = (await res.json().catch(() => null)) as { error?: { message?: string } } | null;
          setStatus("error");
          setError(body?.error?.message ?? `tail failed (HTTP ${res.status})`);
          return;
        }
        setStatus("live");
        await readSse(
          res,
          (m) => {
            if (m.event === "lines") {
              const d = JSON.parse(m.data) as { lines: LogLine[]; dropped?: number };
              recent.current.push({ t: Date.now(), n: d.lines.length });
              if (d.dropped) setDropped((x) => x + d.dropped!);
              if (pausedRef.current) {
                pending.current = pending.current.concat(d.lines).slice(-MAX_BUFFER);
              } else {
                append(d.lines);
              }
            } else if (m.event === "error") {
              const d = JSON.parse(m.data) as { code: string; message: string };
              setStatus("error");
              setError(`${d.code}: ${d.message}`);
            } else if (m.event === "end") {
              const d = m.data ? (JSON.parse(m.data) as { reason?: string }) : {};
              setStatus((s) => (s === "error" ? s : "ended"));
              // The server caps stream length — reconnect transparently.
              if (d.reason === "max-duration") reconnect = setTimeout(() => setAttempt((a) => a + 1), 500);
            }
          },
          ctl.signal,
        );
        setStatus((s) => (s === "live" ? "ended" : s));
      } catch (e) {
        if (ctl.signal.aborted) return;
        setStatus("error");
        setError(e instanceof Error ? e.message : "connection lost");
      }
    })();
    return () => {
      ctl.abort();
      clearTimeout(reconnect);
    };
  }, [query, attempt, append]);

  // lines/s over the last 5 seconds
  useEffect(() => {
    const t = setInterval(() => {
      const cutoff = Date.now() - 5000;
      recent.current = recent.current.filter((x) => x.t >= cutoff);
      setRate(recent.current.reduce((s, x) => s + x.n, 0) / 5);
    }, 1000);
    return () => clearInterval(t);
  }, []);

  const togglePause = () => {
    const next = !paused;
    pausedRef.current = next;
    setPaused(next);
    if (!next && pending.current.length > 0) {
      append(pending.current);
      pending.current = [];
    }
  };

  const tone = status === "live" ? (paused ? "var(--color-warn)" : "var(--color-signal)") : status === "error" ? "var(--color-danger)" : "var(--color-fg-faint)";

  return (
    <div>
      <div className="flex flex-wrap items-center gap-2 border-b border-[var(--color-line)] px-3 py-2 font-mono text-[10px] uppercase tracking-[0.14em]">
        <span className="inline-flex items-center gap-1.5" style={{ color: tone }} role="status" aria-live="polite">
          <span className={`h-1.5 w-1.5 rounded-full ${status === "live" && !paused ? "animate-pulse" : ""}`} style={{ background: tone }} aria-hidden />
          {status === "live" ? (paused ? `paused · ${pending.current.length} buffered` : "live") : status}
        </span>
        <span className="text-[var(--color-fg-faint)]">{rate.toFixed(1)} lines/s</span>
        <span className="text-[var(--color-fg-faint)]">
          {lines.length}/{MAX_BUFFER} in buffer{dropped > 0 ? ` · ${dropped} dropped upstream` : ""}
        </span>
        <div className="ml-auto flex items-center gap-1.5">
          <TailButton onClick={togglePause} label={paused ? "resume" : "pause"} icon={paused ? Play : Pause} />
          <TailButton onClick={() => setFollow((f) => !f)} label={follow ? "following" : "follow"} icon={ArrowDownToLine} active={follow} />
          <TailButton onClick={() => setLines([])} label="clear" icon={Trash2} />
          {(status === "ended" || status === "error") && <TailButton onClick={() => setAttempt((a) => a + 1)} label="reconnect" icon={RotateCw} />}
        </div>
      </div>
      {error && <div className="border-b border-[var(--color-line)] px-3 py-2 font-mono text-[11px] text-[var(--color-danger)]">{error}</div>}
      <LogList
        lines={lines}
        query={query}
        height={height}
        follow={follow && !paused}
        emptyText={status === "connecting" ? "connecting…" : "waiting for new lines matching this query…"}
      />
    </div>
  );
}

function TailButton({
  onClick,
  label,
  icon: Icon,
  active,
}: {
  onClick: () => void;
  label: string;
  icon: React.ComponentType<{ className?: string }>;
  active?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={active}
      className={`inline-flex items-center gap-1.5 border px-2 py-1 transition-colors focus-visible:outline focus-visible:outline-1 focus-visible:outline-[var(--color-cool)] ${
        active ? "border-[var(--color-cool)] text-[var(--color-cool)]" : "border-[var(--color-line-bright)] text-[var(--color-fg-dim)] hover:text-[var(--color-fg)]"
      }`}
    >
      <Icon className="h-3 w-3" aria-hidden />
      {label}
    </button>
  );
}
