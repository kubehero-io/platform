// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import type { Metadata } from "next";
import { GeistSans } from "geist/font/sans";
import "./globals.css";

export const metadata: Metadata = {
  title: "KubeHero · dashboard",
  description: "Operator console for KubeHero — app.kubehero.io",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={GeistSans.variable}>
      <body className="min-h-screen antialiased">{children}</body>
    </html>
  );
}
