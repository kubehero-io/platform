// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { AppStateProvider } from "@/components/app-state";
import { CmdPalette } from "@/components/cmd-palette";
import { KeyboardShortcuts } from "@/components/keyboard-shortcuts";
import { NavStatusProvider } from "@/components/nav-status";
import { Sidebar } from "@/components/sidebar";
import { ToastProvider } from "@/components/toast";

export default function DashboardLayout({ children }: { children: React.ReactNode }) {
  return (
    <AppStateProvider>
      <NavStatusProvider>
        <ToastProvider>
          <a
            href="#main"
            className="sr-only focus:not-sr-only focus:fixed focus:left-3 focus:top-3 focus:z-[60] focus:border focus:border-[var(--color-cool)] focus:bg-[var(--color-bg-raised)] focus:px-3 focus:py-1.5 focus:font-mono focus:text-[12px]"
          >
            skip to content
          </a>
          <CmdPalette />
          <KeyboardShortcuts />
          <div className="flex min-h-screen">
            <Sidebar />
            <main id="main" className="min-w-0 flex-1">
              {children}
            </main>
          </div>
        </ToastProvider>
      </NavStatusProvider>
    </AppStateProvider>
  );
}
