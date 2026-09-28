# syntax=docker/dockerfile:1.7
#
# Collector (node agent: cost, logs, profiles, eBPF). eBPF objects are generated ahead of time (bpf2go) and embedded — no clang here. Built from the REPO ROOT:
#
#   docker build -f services/collector/Dockerfile .
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
COPY services/collector/go.mod services/collector/go.sum services/collector/
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/collector && go mod download

# 2) source
COPY packages/ packages/
COPY services/collector/ services/collector/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/collector && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/collector .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/collector /usr/bin/collector
EXPOSE 8081
USER 65532
ENTRYPOINT ["/usr/bin/collector"]
CMD ["serve"]
