# syntax=docker/dockerfile:1.7
#
# kubehero CLI image (`kubectl run kubehero --image=ghcr.io/kubehero-io/cli`). Built from the REPO ROOT:
#
#   docker build -f cli/kubehero/Dockerfile .
#
# Every module replaces packages/* with the in-repo copies, so the build
# runs outside the Go workspace (GOWORK=off) and only needs packages/ plus
# this module in the context — adding a module elsewhere in the repo never
# breaks this image.

FROM golang:1.26.5-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
ENV GOWORK=off CGO_ENABLED=0

# 1) module files first so dependency downloads cache across source edits
COPY packages/proto/go.mod packages/proto/go.sum packages/proto/
COPY packages/cost-model/go.mod packages/cost-model/
COPY cli/kubehero/go.mod cli/kubehero/go.sum cli/kubehero/
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd cli/kubehero && go mod download

# 2) source
COPY packages/ packages/
COPY cli/kubehero/ cli/kubehero/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd cli/kubehero && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X github.com/kubehero-io/platform/cli/kubehero/cmd.Version=${VERSION}" -o /out/kubehero .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kubehero /usr/bin/kubehero
USER 65532
ENTRYPOINT ["/usr/bin/kubehero"]
