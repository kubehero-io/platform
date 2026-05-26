// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { RootProvider } from "fumadocs-ui/provider";
import type { ReactNode } from "react";
import "./global.css";

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en" suppressHydrationWarning>
      <body>
        <RootProvider>{children}</RootProvider>
      </body>
    </html>
  );
}
