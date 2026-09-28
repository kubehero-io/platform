// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// The bracketed frame around every data block: a hairline header with a
// "///" section label on the left and meta/actions on the right, the body,
// and an optional mono footer (usually the CLI equivalent). Server
// component — interactive children are fine.

import type { ReactNode } from "react";

export function Panel({
  title,
  meta,
  actions,
  footer,
  children,
  className = "",
  bodyClassName = "",
  id,
}: {
  title: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  footer?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
  id?: string;
}) {
  return (
    <section
      id={id}
      className={`bracketed min-w-0 border border-[var(--color-line-bright)] bg-[var(--color-bg-raised)] ${className}`}
    >
      <header className="flex min-h-10 flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-[var(--color-line)] px-4 py-2">
        <h2 className="section-label !text-[var(--color-fg-faint)]">{title}</h2>
        {(meta || actions) && (
          <div className="flex flex-wrap items-center gap-2">
            {meta && (
              <span className="font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">{meta}</span>
            )}
            {actions}
          </div>
        )}
      </header>
      <div className={bodyClassName}>{children}</div>
      {footer && (
        <footer className="flex flex-wrap items-center justify-between gap-2 border-t border-[var(--color-line)] px-4 py-2.5 font-mono text-[10px] uppercase tracking-[0.14em] text-[var(--color-fg-faint)]">
          {footer}
        </footer>
      )}
    </section>
  );
}
