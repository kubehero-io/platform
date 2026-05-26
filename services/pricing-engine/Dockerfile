# syntax=docker/dockerfile:1.7

FROM golang:1.26-alpine AS build
WORKDIR /src
RUN apk add --no-cache git

# Workspace + every module's go.mod (mirrors services/control-plane/Dockerfile;
# see that file for the rationale).
COPY go.work go.work.sum* ./
COPY packages/proto/go.mod                  packages/proto/go.mod
COPY packages/cost-model/go.mod             packages/cost-model/go.mod
COPY services/collector/go.mod              services/collector/go.mod
COPY services/control-plane/go.mod          services/control-plane/go.mod
COPY services/pricing-engine/go.mod         services/pricing-engine/go.mod
COPY services/pricing-engine/go.sum*        services/pricing-engine/
COPY services/operator/go.mod               services/operator/go.mod
COPY cli/kubehero/go.mod                    cli/kubehero/go.mod
COPY infra/dagger/go.mod                    infra/dagger/go.mod

RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/pricing-engine && go mod download -x

COPY packages/proto/                packages/proto/
COPY services/pricing-engine/       services/pricing-engine/

ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/pricing-engine && \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /out/pricing-engine .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pricing-engine /usr/bin/pricing-engine
EXPOSE 8082
USER 65532
ENTRYPOINT ["/usr/bin/pricing-engine"]
CMD ["serve"]
