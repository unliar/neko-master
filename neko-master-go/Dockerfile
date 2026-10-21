# syntax=docker/dockerfile:1

# ---------- build ----------
FROM golang:1.27-alpine AS build

WORKDIR /src

# 依赖先落地，利用缓存
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# 纯 Go，无 CGO，可直接交叉编译
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/neko ./cmd/neko

# ---------- runtime ----------
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -g 1000 -S neko \
 && adduser -u 1000 -S neko -G neko \
 && mkdir -p /app/data \
 && chown -R neko:neko /app

WORKDIR /app

COPY --from=build /out/neko /usr/local/bin/neko

USER neko

ENV DB_PATH=/app/data/stats.db \
    API_PORT=3001 \
    LOG_LEVEL=info \
    TZ=Asia/Shanghai

VOLUME ["/app/data"]

EXPOSE 3001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${API_PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/neko"]
