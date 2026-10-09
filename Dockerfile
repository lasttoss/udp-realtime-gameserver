# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/relay ./cmd/relay

FROM alpine:3.20
RUN apk add --no-cache wget && adduser -D -u 10001 app
WORKDIR /app
COPY --from=builder /out/relay /app/relay
USER app
EXPOSE 8080/tcp 9000/udp
HEALTHCHECK --interval=10s --timeout=3s --start-period=3s --retries=5 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/app/relay"]
