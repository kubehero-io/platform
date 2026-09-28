// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) KubeHero contributors

/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // Produces a minimal self-contained runtime under .next/standalone so the
  // container image doesn't need node_modules at rest. See Dockerfile.
  output: "standalone",
  // Baseline hardening headers for every route. No CSP here: the App
  // Router's inline bootstrap scripts need per-request nonces, which is a
  // proxy.ts concern for a later pass.
  async headers() {
    return [
      {
        source: "/:path*",
        headers: [
          { key: "X-Content-Type-Options", value: "nosniff" },
          { key: "X-Frame-Options", value: "DENY" },
          { key: "Referrer-Policy", value: "strict-origin-when-cross-origin" },
          {
            key: "Permissions-Policy",
            value: "camera=(), microphone=(), geolocation=(), payment=()",
          },
        ],
      },
    ];
  },
};
export default nextConfig;
