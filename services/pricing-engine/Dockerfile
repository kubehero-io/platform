# syntax=docker/dockerfile:1.7
#
# Pricing engine (live cloud price lists). Built from the REPO ROOT:
#
#   docker build -f services/pricing-engine/Dockerfile .
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
COPY services/pricing-engine/go.mod services/pricing-engine/go.sum services/pricing-engine/
RUN --mount=type=cache,target=/root/go/pkg/mod \
    cd services/pricing-engine && go mod download

# 2) source
COPY packages/ packages/
COPY services/pricing-engine/ services/pricing-engine/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    cd services/pricing-engine && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w" -o /out/pricing-engine .

# ─── runtime ───────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/pricing-engine /usr/bin/pricing-engine
EXPOSE 8082
USER 65532
ENTRYPOINT ["/usr/bin/pricing-engine"]
CMD ["serve"]
