# syntax=docker/dockerfile:1.7
#
# Multi-stage build for cmd/ingest.
#
# Stage 1: build a static Go binary from the module rooted at the
# repository top level. The build context (set in docker-compose.yml)
# is the repo root, not the cmd/ingest directory.
# Stage 2: ship a minimal distroless image with only the binary.

FROM golang:1.24-alpine AS build

WORKDIR /src

# Cache the module graph independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

# Source.
COPY cmd ./cmd
COPY internal ./internal

# Static, stripped, no symbols. CGO disabled — we don't link any C deps.
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -trimpath \
      -o /out/ingest \
      ./cmd/ingest

FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/ingest /ingest

USER nonroot:nonroot
EXPOSE 2055/udp 6343/udp 9100/tcp

ENTRYPOINT ["/ingest"]
