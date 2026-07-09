// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Server-safe renderer for the advisor's markdown briefing body. All the
// parsing lives in lib/markdown-lite.ts (pure, unit-tested); this file only
// maps blocks to styled JSX.

import { parseMarkdown, type InlineSpan } from "@/lib/markdown-lite";

function Spans({ spans }: { spans: InlineSpan[] }) {
  return (
    <>
      {spans.map((s, i) => {
        switch (s.type) {
          case "bold":
            return (
              <strong key={i} className="font-medium text-[var(--color-fg)]">
                {s.text}
              </strong>
            );
          case "italic":
            return (
              <em key={i} className="italic">
                {s.text}
              </em>
            );
          case "code":
            return (
              <code
                key={i}
                className="rounded-[2px] border border-[var(--color-line)] bg-[var(--color-bg-sunken)] px-1 py-px font-mono text-[0.92em] text-[var(--color-cool)]"
              >
                {s.text}
              </code>
            );
          default:
            return <span key={i}>{s.text}</span>;
        }
      })}
    </>
  );
}

export function BriefingMarkdown({ markdown }: { markdown: string }) {
  const blocks = parseMarkdown(markdown);
  return (
    <div className="flex flex-col gap-3.5 text-[13.5px] leading-relaxed text-[var(--color-fg-dim)]">
      {blocks.map((b, i) => {
        switch (b.type) {
          case "heading":
            return (
              <div
                key={i}
                className="mt-2 flex items-center gap-2 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)] first:mt-0"
              >
                ///&nbsp;
                <Spans spans={b.spans} />
              </div>
            );
          case "list":
            return b.ordered ? (
              <ol key={i} className="flex list-none flex-col gap-1.5">
                {b.items.map((item, j) => (
                  <li key={j} className="flex gap-2.5">
                    <span className="shrink-0 font-mono text-[10.5px] tabular-nums leading-[1.7] text-[var(--color-fg-faint)]">
                      {String(j + 1).padStart(2, "0")}
                    </span>
                    <span>
                      <Spans spans={item} />
                    </span>
                  </li>
                ))}
              </ol>
            ) : (
              <ul key={i} className="flex list-none flex-col gap-1.5">
                {b.items.map((item, j) => (
                  <li key={j} className="flex gap-2.5">
                    <span
                      className="mt-[0.62em] h-1 w-1 shrink-0"
                      style={{ background: "var(--color-fg-faint)" }}
                      aria-hidden
                    />
                    <span>
                      <Spans spans={item} />
                    </span>
                  </li>
                ))}
              </ul>
            );
          case "code":
            return (
              <pre
                key={i}
                className="overflow-x-auto border border-[var(--color-line)] bg-[var(--color-bg-sunken)] p-3 font-mono text-[11.5px] leading-relaxed text-[var(--color-fg-dim)]"
              >
                {b.text}
              </pre>
            );
          default:
            return (
              <p key={i}>
                <Spans spans={b.spans} />
              </p>
            );
        }
      })}
    </div>
  );
}
