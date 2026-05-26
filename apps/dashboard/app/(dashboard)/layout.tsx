// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { AppStateProvider } from "@/components/app-state";
import { CmdPalette } from "@/components/cmd-palette";
import { Sidebar } from "@/components/sidebar";
import { ToastProvider } from "@/components/toast";

export default function DashboardLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <AppStateProvider>
      <ToastProvider>
        <CmdPalette />
        <div className="flex min-h-screen">
          <Sidebar />
          <main className="min-w-0 flex-1">{children}</main>
        </div>
      </ToastProvider>
    </AppStateProvider>
  );
}
