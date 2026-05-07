# syntax=docker/dockerfile:1.7
# Multi-stage build for cmd/alert. Mirrors cmd/ingest and cmd/api.

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
      -o /out/alert \
      ./cmd/alert

FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/alert /alert

USER nonroot:nonroot
EXPOSE 9101/tcp

ENTRYPOINT ["/alert"]
