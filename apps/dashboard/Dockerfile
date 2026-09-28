# syntax=docker/dockerfile:1.7
# Next.js standalone output for minimal runtime image. Expects to be
# built from the monorepo root (so pnpm workspace resolution works):
#
#   docker build -f apps/dashboard/Dockerfile -t kubehero/dashboard .
#
# Node 24 is the current LTS line (Next 16 needs >= 20.9).

FROM node:24-alpine AS base
RUN corepack enable && corepack prepare pnpm@9.15.0 --activate
WORKDIR /repo

# ─── deps ──────────────────────────────────────────────────────────────────
FROM base AS deps
COPY pnpm-workspace.yaml pnpm-lock.yaml* package.json ./
COPY apps/dashboard/package.json apps/dashboard/package.json
# Workspace dependency of the dashboard (tsconfig presets).
COPY packages/tsconfig packages/tsconfig
RUN --mount=type=cache,id=pnpm-cache,target=/pnpm-store \
    pnpm config set store-dir /pnpm-store && \
    pnpm install --frozen-lockfile --filter @kubehero/dashboard...

# ─── build ─────────────────────────────────────────────────────────────────
FROM base AS build
COPY --from=deps /repo/node_modules ./node_modules
COPY --from=deps /repo/apps/dashboard/node_modules ./apps/dashboard/node_modules
COPY pnpm-workspace.yaml pnpm-lock.yaml* package.json ./
COPY packages/tsconfig packages/tsconfig
COPY apps/dashboard ./apps/dashboard
ENV NEXT_TELEMETRY_DISABLED=1
RUN pnpm -F @kubehero/dashboard build

# ─── runtime ───────────────────────────────────────────────────────────────
# distroless/nodejs24-debian12 instead of node:24-alpine cuts the image
# ~40% and removes the shell + busybox + apk surface. The Next.js
# standalone bundle is just a server.js + node_modules slice, which
# distroless runs directly. The `nonroot` tag ships uid/gid 65532
# baked in so we don't need our own adduser shim.
FROM gcr.io/distroless/nodejs24-debian12:nonroot AS runtime
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=3001
ENV HOSTNAME=0.0.0.0

COPY --from=build --chown=65532:65532 /repo/apps/dashboard/public ./apps/dashboard/public
COPY --from=build --chown=65532:65532 /repo/apps/dashboard/.next/standalone ./
COPY --from=build --chown=65532:65532 /repo/apps/dashboard/.next/static ./apps/dashboard/.next/static

EXPOSE 3001
# Numeric, like every KubeHero image: with runAsNonRoot the kubelet can
# only verify a numeric user (65532 is distroless's nonroot).
USER 65532:65532
# distroless/nodejs has `node` as the entrypoint; pass server.js as
# the argument.
CMD ["apps/dashboard/server.js"]
