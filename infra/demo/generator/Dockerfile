# syntax=docker/dockerfile:1.7
#
# Demo telemetry generator (see main.go). Built from the REPO ROOT:
#   docker build -f infra/demo/generator/Dockerfile .

FROM golang:1.26.8-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY packages/proto/go.mod packages/proto/go.sum packages/proto/
COPY infra/demo/generator/go.mod infra/demo/generator/go.sum infra/demo/generator/
RUN --mount=type=cache,target=/root/go/pkg/mod cd infra/demo/generator && go mod download
COPY packages/proto/ packages/proto/
COPY infra/demo/generator/ infra/demo/generator/
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/go/pkg/mod \
    cd infra/demo/generator && GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/generator .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/generator /usr/bin/generator
USER 65532
ENTRYPOINT ["/usr/bin/generator"]
