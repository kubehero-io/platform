# syntax=docker/dockerfile:1.7

# ─── build ─────────────────────────────────────────────────────────────────
FROM golang:1.26-alpine AS build
WORKDIR /src

# Module files first for better layer caching.
COPY services/collector/go.mod services/collector/go.sum* ./
RUN go mod download

# Source.
COPY services/collector/ ./

# Static linux/amd64 build — CGO disabled so the binary runs on distroless.
ARG TARGETARCH=amd64
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -trimpath -o /out/collector .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/collector /usr/bin/collector
EXPOSE 8081
USER 65532
ENTRYPOINT ["/usr/bin/collector"]
CMD ["serve"]
