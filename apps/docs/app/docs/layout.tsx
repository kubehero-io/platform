// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

import { DocsLayout } from "fumadocs-ui/layouts/docs";
import type { ReactNode } from "react";
import { source } from "@/lib/source";

export default function Layout({ children }: { children: ReactNode }) {
  return (
    <DocsLayout
      tree={source.pageTree}
      nav={{ title: "KubeHero docs" }}
      githubUrl="https://github.com/kubehero-io/platform"
    >
      {children}
    </DocsLayout>
  );
}
