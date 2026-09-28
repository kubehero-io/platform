# syntax=docker/dockerfile:1.7
#
# Build the operator manager. Built from the REPO ROOT inside the Go
# workspace (packages/proto resolves locally):
#
#   docker build -f services/operator/Dockerfile .
#
# Runtime is distroless/static-debian12:nonroot — no shell, no package
# manager, no glibc: just the binary + ca-certs + nsswitch.

FROM golang:1.26.5-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN apk add --no-cache git

COPY go.work go.work.sum* ./
COPY packages/proto/go.mod                  packages/proto/go.mod
COPY packages/cost-model/go.mod             packages/cost-model/go.mod
COPY services/advisor/go.mod                services/advisor/go.mod
COPY services/collector/go.mod              services/collector/go.mod
COPY services/control-plane/go.mod          services/control-plane/go.mod
COPY services/pricing-engine/go.mod         services/pricing-engine/go.mod
COPY services/operator/go.mod               services/operator/go.mod
COPY services/operator/go.sum               services/operator/go.sum
COPY cli/kubehero/go.mod                    cli/kubehero/go.mod
COPY infra/dagger/go.mod                    infra/dagger/go.mod

RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/operator && go mod download

COPY packages/proto/        packages/proto/
COPY services/operator/     services/operator/

RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/operator && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /out/manager ./cmd

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /
COPY --from=builder /out/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
