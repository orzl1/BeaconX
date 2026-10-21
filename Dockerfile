# syntax=docker/dockerfile:1

# ---- 构建阶段 ----
FROM golang:1.24-alpine AS build

RUN apk add --no-cache git ca-certificates

WORKDIR /src

# 依赖下载与源码变更分开缓存。
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w \
        -X github.com/orzl1/BeaconX/internal/version.Version=${VERSION} \
        -X github.com/orzl1/BeaconX/internal/version.Commit=${COMMIT} \
        -X github.com/orzl1/BeaconX/internal/version.BuildTime=${BUILD_TIME}" \
      -o /out/beaconx ./cmd/beaconx

# ---- 运行阶段 ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -g 10001 beaconx \
    && adduser -D -u 10001 -G beaconx -h /var/lib/beaconx beaconx \
    && mkdir -p /var/lib/beaconx \
    && chown -R beaconx:beaconx /var/lib/beaconx

COPY --from=build /out/beaconx /usr/local/bin/beaconx

USER beaconx
WORKDIR /var/lib/beaconx

ENV BEACONX_ADDR=:6969 \
    BEACONX_HOME=/var/lib/beaconx

EXPOSE 6969

HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=3 \
    CMD ["/usr/local/bin/beaconx", "healthcheck", "-addr", "http://127.0.0.1:6969/readyz"]

ENTRYPOINT ["/usr/local/bin/beaconx", "serve"]
