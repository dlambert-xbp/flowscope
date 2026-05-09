# syntax=docker/dockerfile:1.7
#
# Multi-stage build for cmd/init.
#
# One-shot bootstrap container: applies forward-only schema migrations
# and emits ALTER TABLE … MODIFY TTL statements for the operator-
# controlled retention knobs. Mirrors cmd/api / cmd/ingest / cmd/alert.

FROM golang:1.25-alpine AS build

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
      -o /out/init \
      ./cmd/init

FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/init /init

USER nonroot:nonroot

ENTRYPOINT ["/init"]
