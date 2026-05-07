# syntax=docker/dockerfile:1.7
# Multi-stage build for cmd/snmp.

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
      -o /out/snmp \
      ./cmd/snmp

FROM gcr.io/distroless/static:nonroot AS runtime

COPY --from=build /out/snmp /snmp

USER nonroot:nonroot
EXPOSE 9102/tcp

ENTRYPOINT ["/snmp"]
