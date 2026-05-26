# syntax=docker/dockerfile:1.7
#
# Multi-stage build for the control-plane.
#
# We build inside the Go workspace (go.work) so cross-module imports —
# specifically packages/proto/gen/* — resolve without each service
# pinning a `replace` directive. Layer order is mod-files-first so a
# source change doesn't invalidate the dep download cache.

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git

# 1) workspace + every module's go.mod (cheap, rarely changes)
COPY go.work go.work.sum* ./
COPY packages/proto/go.mod                  packages/proto/go.mod
COPY packages/cost-model/go.mod             packages/cost-model/go.mod
COPY services/collector/go.mod              services/collector/go.mod
COPY services/control-plane/go.mod          services/control-plane/go.mod
COPY services/control-plane/go.sum*         services/control-plane/
COPY services/pricing-engine/go.mod         services/pricing-engine/go.mod
COPY services/operator/go.mod               services/operator/go.mod
COPY cli/kubehero/go.mod                    cli/kubehero/go.mod
COPY infra/dagger/go.mod                    infra/dagger/go.mod

# 2) prime the module cache for what control-plane actually needs
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/control-plane && go mod download -x

# 3) source — proto first (changes more often than mod), then service
COPY packages/proto/                packages/proto/
COPY services/control-plane/        services/control-plane/

ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/control-plane && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /out/control-plane .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /usr/bin/control-plane
EXPOSE 8080
USER 65532
ENTRYPOINT ["/usr/bin/control-plane"]
CMD ["serve"]
