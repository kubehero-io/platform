/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // Produces a minimal self-contained runtime under .next/standalone so the
  // container image doesn't need node_modules at rest. See Dockerfile.
  output: "standalone",
};
export default nextConfig;
