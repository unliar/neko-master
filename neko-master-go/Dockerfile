# syntax=docker/dockerfile:1
#
# neko-master 单镜像构建：前端静态产物 + Go 二进制，一个镜像全栈。
#
# ⚠️ build context 必须是【仓库根目录】，因为前端是 pnpm monorepo：
#   - apps/web 依赖 packages/shared（workspace:*）
#   - apps/web/next.config.ts 会读根 package.json 拿版本号
# 所以不能只在 neko-master-go/ 下 build。用法：
#
#   docker build -f neko-master-go/Dockerfile -t neko-master-go .
#
# 四个阶段：web-deps → web-build → go-build → runtime

# ---------- 1. 前端依赖 ----------
FROM node:24-alpine AS web-deps

# 用 npm 全局装 pnpm，而非 corepack prepare —— 后者要从 npm registry
# 下载 pnpm tarball，在国内网络下常超时失败。版本与根 package.json 的
# packageManager 字段保持一致。
RUN npm install -g pnpm@9.15.9

WORKDIR /repo

# 先拷依赖清单再 install，最大化层缓存
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY packages/shared/package.json ./packages/shared/
COPY apps/web/package.json ./apps/web/

RUN pnpm install --frozen-lockfile

# ---------- 2. 前端构建 ----------
FROM web-deps AS web-build

WORKDIR /repo

# 整目录拷贝而非逐个列文件：前端构建需要 postcss.config.mjs、
# components.json、types/、next-env.d.ts 等一堆零散配置，
# 逐个列迟早漏。node_modules 与 .next 已被 .dockerignore 排除，
# 所以 context 不会因此变大（依赖由 web-deps 阶段装好）。
COPY packages/shared/ ./packages/shared/
COPY apps/web/ ./apps/web/

# shared 是 TS 源码，需要先编译出 dist 供 web 引用
RUN pnpm --filter @neko-master/shared build

# 静态导出。NODE_ENV=production 会启用 PWA 插件（生成 sw.js）。
# ⚠️ 不要设成 development —— 那会让 React dev/prod 版本混用，
# 抛 "Cannot read properties of null (reading 'useContext')"。
ENV NODE_ENV=production
RUN pnpm --filter @neko-master/web build

# ---------- 3. Go 构建 ----------
FROM golang:1.27-alpine AS go-build

WORKDIR /src

COPY neko-master-go/go.mod neko-master-go/go.sum ./

# 国内网络直连 proxy.golang.org 常 TLS 超时，走 goproxy.cn 镜像。
# GitHub Actions 在新加坡/美国节点，用默认代理反而更稳，所以只在
# 本地构建时通过 build-arg 开启（见 Makefile 的 docker target）。
ARG GOPROXY=https://goproxy.cn,direct
ENV GOPROXY=${GOPROXY}
RUN go mod download

COPY neko-master-go/ ./

# 前端静态产物拷进 embed 目录。
# //go:embed 只能嵌【包目录下】的文件，所以必须放 internal/api/web/out。
# 仓库里该目录只有 .gitkeep（产物不进 git），这里由 Docker 填充。
COPY --from=web-build /repo/apps/web/out ./internal/api/web/out

ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/neko ./cmd/neko

# ---------- 4. 运行时 ----------
FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata wget \
 && addgroup -g 1000 -S neko \
 && adduser -u 1000 -S neko -G neko \
 && mkdir -p /app/data \
 && chown -R neko:neko /app

WORKDIR /app

COPY --from=go-build /out/neko /usr/local/bin/neko

USER neko

# 单镜像模式下 UI 与 API 同源，一个端口提供全部：
#   /        → 前端 UI（302 到 /zh/）
#   /api/*   → REST API
#   /ws      → WebSocket
ENV DB_PATH=/app/data/stats.db \
    API_PORT=3001 \
    LOG_LEVEL=info \
    TZ=Asia/Shanghai

VOLUME ["/app/data"]

EXPOSE 3001

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${API_PORT}/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/neko"]
