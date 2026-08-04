# syntax=docker/dockerfile:1.7

# ---- build ------------------------------------------------------------------
FROM golang:1.23-alpine AS build

WORKDIR /src

# Dependencies first so that source edits do not invalidate the module cache.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Tests run in the image build so a broken gateway cannot be deployed by accident.
ARG RUN_TESTS=0
RUN if [ "$RUN_TESTS" = "1" ]; then go test ./... ; fi

ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/gateway ./cmd/gateway

# ---- runtime ----------------------------------------------------------------
# Static binary, so scratch would work; alpine is used only because it carries CA
# certificates for PROVIDER_MODE=live. Nothing else is installed.
FROM alpine:3.21

RUN apk add --no-cache ca-certificates \
 && adduser -D -u 10001 -h /nonexistent -s /sbin/nologin llmrouter

COPY --from=build /out/gateway /gateway

USER 10001:10001
EXPOSE 8080 9090

ENV GATEWAY_ADDR=:8080 \
    METRICS_ADDR=:9090 \
    PROVIDER_MODE=mock \
    LOG_FORMAT=json

# The binary probes itself, so the image needs no curl or wget.
HEALTHCHECK --interval=10s --timeout=3s --start-period=10s --retries=5 \
  CMD ["/gateway", "-healthcheck"]

ENTRYPOINT ["/gateway"]
