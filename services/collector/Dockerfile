# syntax=docker/dockerfile:1.7
#
# Multi-stage build for the collector (node agent). Built from the repo
# root inside the Go workspace so packages/proto and packages/cost-model
# resolve locally. The eBPF objects are generated ahead of time (bpf2go)
# and embedded, so no clang is needed here.

FROM golang:1.26.5-alpine AS build
WORKDIR /src
RUN apk add --no-cache git

COPY go.work go.work.sum* ./
COPY packages/proto/go.mod                  packages/proto/go.mod
COPY packages/cost-model/go.mod             packages/cost-model/go.mod
COPY services/advisor/go.mod                services/advisor/go.mod
COPY services/collector/go.mod              services/collector/go.mod
COPY services/collector/go.sum*             services/collector/
COPY services/control-plane/go.mod          services/control-plane/go.mod
COPY services/pricing-engine/go.mod         services/pricing-engine/go.mod
COPY services/operator/go.mod               services/operator/go.mod
COPY cli/kubehero/go.mod                    cli/kubehero/go.mod
COPY infra/dagger/go.mod                    infra/dagger/go.mod

RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/collector && go mod download -x

COPY packages/proto/                packages/proto/
COPY packages/cost-model/           packages/cost-model/
COPY services/collector/            services/collector/

ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/collector && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${VERSION}" -trimpath -o /out/collector .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /usr/bin/collector
EXPOSE 8081
USER 65532
ENTRYPOINT ["/usr/bin/collector"]
CMD ["serve"]
