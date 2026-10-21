# neko-master-go

[nkeko-master](../) 的 Go 后端重写。前端代码**一行不改**，Go 服务严格实现同一套 HTTP API 契约。

## 为什么

上游后端是 Fastify + TypeScript（约 26k 行），依赖 better-sqlite3（含原生编译）、maxmind、undici、ws。  
Go 版本：

- 单二进制，`CGO_ENABLED=0` 交叉编译
- 纯 Go SQLite（`modernc.org/sqlite`），无需编译原生模块
- 零第三方框架（标准库 `net/http` + Go 1.22 路由）
- 可直接读取上游 SQLite 库，表结构完全一致

## 状态

Phase 1 已完成，可实跑：

| 能力 | 状态 |
|---|---|
| 网关连接（`/connections` 全量快照） | ✅ |
| 增量计算（preexisting 宽限 / 计数器回退 / 连接数只计一次） | ✅ 8 个单测覆盖 |
| 13 张聚合表 UPSERT | ✅ |
| `GET /health` | ✅ |
| `GET /api/stats/summary` | ✅ |
| `GET /api/stats/global` | ✅ |
| `GET /api/stats/domains` `/ips` `/proxies` `/rules` | ✅ |
| `GET /api/stats/devices` `/countries` `/hourly` `/trend` | ✅ |
| `GET /api/gateway/proxies` `/rules` `/providers/proxies` | ✅ |
| `GET /api/backends/` `/active` `/listening` | ✅ |

Phase 2 待做：agent 上报协议、鉴权、`/api/db/*` 运维、WebSocket、GeoIP、下钻维度接口、ClickHouse。

完整契约见 [`../API-DETAILS.md`](../API-DETAILS.md)（2650 行，从上游源码逐函数提取）。

## 快速开始

```bash
# 从源码跑
go run ./cmd/neko \
  -seed-name home \
  -seed-url 192.168.31.1:9090 \
  -seed-token YOUR_MIHOMO_SECRET

# 编译
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o neko ./cmd/neko
```

启动后默认监听 `:3001`：

```bash
curl localhost:3001/health
curl localhost:3001/api/stats/summary
curl localhost:3001/api/stats/global
```

## 环境变量

命名与上游完全一致，`docker-compose.yml` 可直接复用。

| 变量 | 默认 | 说明 |
|---|---|---|
| `API_PORT` | `3001` | HTTP 监听端口 |
| `DB_PATH` | `./data/stats.db` | SQLite 路径 |
| `LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FLUSH_INTERVAL_MS` | `30000` | 缓冲刷盘间隔 |
| `FLUSH_MAX_BUFFER_SIZE` | `5000` | 缓冲区上限（达到即刷） |
| `BACKEND_TLS_INSECURE` | — | 设 `1` 跳过网关 TLS 校验 |
| `SHOWCASE_SITE_MODE` | — | 设 `1` 进入只读展示模式 |
| `SEED_BACKEND_NAME` | — | 首次启动写入的 backend 名 |
| `SEED_BACKEND_URL` | — | 首次启动写入的网关地址 |
| `SEED_BACKEND_TOKEN` | — | 首次启动写入的网关 secret |

命令行参数 `-seed-name` / `-seed-url` / `-seed-token` / `-poll-ms` 优先级更高。

## 与上游共存

```
neko-master/                 ← fork，前端不动
├── apps/                    ← 上游代码，不修改
├── packages/
└── neko-master-go/          ← 本次新增
    ├── cmd/neko/
    └── internal/
```

**只新增 `neko-master-go/` 目录**，不改动 `apps/` 与 `packages/`。这样上游更新时 `git merge upstream/main` 不会冲突，Go 后端按 `API-DETAILS.md` 契约补齐即可。

## 已知差异

| 项 | 上游行为 | Go 实现 | 原因 |
|---|---|---|---|
| `/api/stats/connections` | 恒返回 `[]`（标了 `@deprecated`） | 暂未实现 | 上游已废弃 |
| 实时叠加 | DTO 会叠加内存 buffer | 只读已落库数据 | 待补；当前会晚最多 30 秒看到最新数据 |
| `country_stats` | 两处定义冲突 | 按 `shared` 版（含可选 `lastSeen`） | 与前端一致 |
| ClickHouse | 支持双写与查询路由 | 未实现 | SQLite 足够；后续可选 |
| Surge 网关 | 支持 HTTP 轮询 | 未实现 | Phase 2 |

## 测试

```bash
go test ./...
```

`internal/collector/tracker_test.go` 覆盖：首次快照不计量、增量做差、连接数只计一次、计数器回退、连接消失清理、零增量跳过、链路拼接、UTC 分桶。
