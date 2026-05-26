# syntax=docker/dockerfile:1.7
# The CLI is primarily distributed via brew / apt / GitHub Releases, but
# we also ship a container image so customers can `kubectl run kubehero
# --image=ghcr.io/kubehero-io/cli` for ad-hoc commands inside
# their cluster.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY cli/kubehero/go.mod cli/kubehero/go.sum* cli/kubehero/
RUN cd cli/kubehero && go mod download

COPY cli/kubehero/ cli/kubehero/

ARG VERSION=0.0.0-dev
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    cd cli/kubehero && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X github.com/kubehero-io/platform/cli/kubehero/cmd.Version=${VERSION}" \
      -trimpath -o /out/kubehero .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/kubehero /usr/bin/kubehero
USER 65532
ENTRYPOINT ["/usr/bin/kubehero"]
