# syntax=docker/dockerfile:1.7
#
# Multi-stage build for cmd/api. Mirrors cmd/ingest/Dockerfile.
#
# Build context is the repo root, not cmd/api.

FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -trimpath \
      -o /out/api \
      ./cmd/api

FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/api /api

USER nonroot:nonroot
EXPOSE 8080/tcp

ENTRYPOINT ["/api"]
