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
  {
    rules: {
      // "/// section" is the dashboard's typographic section marker, used
      // as literal text in panel titles and eyebrows. The rule exists to
      // catch `// comment` accidentally rendered as text; with the marker
      // everywhere it would only produce noise (or 50+ `{"///"}` wrappers).
      "react/jsx-no-comment-textnodes": "off",
      // `const { dropped: _x, ...rest } = o` is the idiom for omitting a key.
      "@typescript-eslint/no-unused-vars": [
        "warn",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_", ignoreRestSiblings: true },
      ],
    },
  },
  globalIgnores([".next/**", "out/**", "build/**", "coverage/**", "next-env.d.ts"]),
]);
