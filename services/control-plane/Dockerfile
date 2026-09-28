# syntax=docker/dockerfile:1.7
#
# Control plane (API, policy engine, signal engines). Built from the REPO ROOT:
#
#   docker build -f services/control-plane/Dockerfile .
#
# Every module replaces packages/* with the in-repo copies, so the build
# runs outside the Go workspace (GOWORK=off) and only needs packages/ plus
# this module in the context — adding a module elsewhere in the repo never
# breaks this image.

FROM golang:1.26.8-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
ENV GOWORK=off CGO_ENABLED=0

# 1) module files first so dependency downloads cache across source edits
COPY packages/proto/go.mod packages/proto/go.sum packages/proto/
COPY packages/cost-model/go.mod packages/cost-model/
COPY services/control-plane/go.mod services/control-plane/go.sum services/control-plane/
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/control-plane && go mod download

# 2) source
COPY packages/ packages/
COPY services/control-plane/ services/control-plane/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/control-plane && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/kubehero-io/platform/services/control-plane/internal/rpc.Version=${VERSION}" -o /out/control-plane .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /usr/bin/control-plane
EXPOSE 8080
USER 65532
ENTRYPOINT ["/usr/bin/control-plane"]
CMD ["serve"]
