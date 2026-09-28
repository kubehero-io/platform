// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

// Next 16 removed `next lint`; the ESLint CLI runs this flat config
// directly (`pnpm --filter @kubehero/dashboard lint`).

import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

export default defineConfig([
  ...nextVitals,
  ...nextTs,
  globalIgnores([".next/**", "out/**", "build/**", "coverage/**", "next-env.d.ts"]),
]);
