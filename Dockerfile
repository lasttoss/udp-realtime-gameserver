# syntax=docker/dockerfile:1

# The build stage honours TARGETOS/TARGETARCH, so it builds the right binary without needing a
# builder for the target architecture. With the default builder the arguments are empty and Go
# uses the architecture it is running on; under `docker buildx build --platform linux/arm64` they
# hold the values buildx passes in. Nothing here needs BuildKit-specific syntax, so
# `docker compose build` works with either builder.
FROM golang:1.24-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay

FROM alpine:3.20
RUN apk add --no-cache wget && adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /out/relay /app/relay
USER app
EXPOSE 8080/tcp 9000/udp
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/relay"]
