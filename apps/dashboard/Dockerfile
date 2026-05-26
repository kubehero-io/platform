# syntax=docker/dockerfile:1.7
# Next.js standalone output for minimal runtime image. Expects to be
# built from the monorepo root (so pnpm workspace resolution works).

FROM node:22-alpine AS base
RUN corepack enable && corepack prepare pnpm@9.15.0 --activate
WORKDIR /repo

# ─── deps ──────────────────────────────────────────────────────────────────
FROM base AS deps
COPY pnpm-workspace.yaml pnpm-lock.yaml* package.json ./
COPY apps/dashboard/package.json apps/dashboard/package.json
RUN --mount=type=cache,id=pnpm-cache,target=/pnpm-store \
    pnpm config set store-dir /pnpm-store && \
    pnpm install --frozen-lockfile --filter @kubehero/dashboard...

# ─── build ─────────────────────────────────────────────────────────────────
FROM base AS build
COPY --from=deps /repo/node_modules ./node_modules
COPY --from=deps /repo/apps/dashboard/node_modules ./apps/dashboard/node_modules
COPY pnpm-workspace.yaml pnpm-lock.yaml* package.json ./
COPY apps/dashboard ./apps/dashboard
ENV NEXT_TELEMETRY_DISABLED=1
RUN pnpm -F @kubehero/dashboard build

# ─── runtime ───────────────────────────────────────────────────────────────
# distroless/nodejs22-debian12 instead of node:22-alpine cuts the image
# ~40% and removes the shell + busybox + apk surface. The Next.js
# standalone bundle is just a server.js + node_modules slice, which
# distroless runs directly. The `nonroot` tag ships uid/gid 65532
# baked in so we don't need our own adduser shim.
FROM gcr.io/distroless/nodejs22-debian12:nonroot AS runtime
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3001
ENV HOSTNAME=0.0.0.0

COPY --from=build --chown=65532:65532 /repo/apps/dashboard/public ./public
COPY --from=build --chown=65532:65532 /repo/apps/dashboard/.next/standalone ./
COPY --from=build --chown=65532:65532 /repo/apps/dashboard/.next/static ./apps/dashboard/.next/static

EXPOSE 3001
USER nonroot
# distroless/nodejs22 has `node` as the entrypoint; pass server.js as
# the argument.
CMD ["apps/dashboard/server.js"]
