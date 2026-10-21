# neko-master-go

[neko-master](../) 的 Go 后端重写。前端代码**一行不改**，Go 服务严格实现同一套 HTTP API 契约。

## 为什么

上游后端是 Fastify + TypeScript（约 26k 行），依赖 better-sqlite3（含原生编译）、maxmind、undici、ws。
Go 版本：

- 单二进制，`CGO_ENABLED=0` 交叉编译（arm64 实测 11 MB）
- 纯 Go SQLite（`modernc.org/sqlite`），无需编译原生模块
- 零第三方框架（标准库 `net/http` + Go 1.22 路由 + 手写 RFC 6455）
- 可直接读取上游 SQLite 库，30 张表结构完全一致

## 状态

**Phase 1 与 Phase 2 均已完成**，共 **66 条路由**，覆盖上游契约的全部功能域。

| 能力 | 状态 |
|---|---|
| 网关连接（`/connections` 全量快照，Bearer 鉴权） | ✅ |
| 增量计算（preexisting 宽限 / 计数器回退 / 连接数只计一次） | ✅ 8 个单测覆盖 |
| 内存缓冲 + 定时刷盘（默认 30s / 5000 条） | ✅ |
| 20 张聚合表 UPSERT（含 `ips/rules/chains` 的 `INSTR` 去重与 4000 字符上限） | ✅ |
| `GET /health` | ✅ |
| `GET /api/stats/summary` | ✅ |
| `GET /api/stats/global` | ✅ |
| `GET /api/stats/domains` `/ips` `/proxies` `/rules` | ✅ |
| `GET /api/stats/devices` `/countries` `/hourly` `/trend` | ✅ |
| `GET /api/stats/trend/aggregated`（`minutes`/`bucketMinutes`/`tzOffsetMinutes` 三参数，按桶聚合，按本地日对齐） | ✅ |
| 下钻接口 15 条（domains/ips/proxies/rules/devices 各维度 + proxy-stats + ip-details） | ✅ |
| `GET /api/stats/rules/chain-flow` | ✅ |
| `GET /api/stats/rules/chain-flow-all` | ✅ |
| `GET /api/stats/rule-proxy-map` | ✅ |
| `GET /api/stats/connections`（上游 `@deprecated`，恒返回 `[]`） | ✅ |
| `GET /api/gateway/proxies` `/rules` `/providers/proxies` | ✅ |
| `POST /api/gateway/providers/proxies/refresh` | ✅ |
| `GET /api/backends` `/active` `/listening` `/health/history` | ✅ |
| `POST /api/backends` `PUT/DELETE /api/backends/{id}` | ✅ |
| `POST /api/backends/{id}/activate` `/listening` `/clear-data` `/test` | ✅ |
| `POST /api/backends/{id}/rotate-agent-token` `POST /api/backends/test` | ✅ |
| Agent 上报 4 条（`heartbeat` / `report` / `config` / `policy-state`） | ✅ 四级鉴权 + 恒定时间比对 |
| 访问控制 6 条（`state` / `enable` / `disable` / `verify` / `token` / `logout`） ✅ 全局中间件 |
| `GET/PUT /api/db/retention` `/geoip` | ✅ |
| `GET /api/db/stats` | ✅ |
| `POST /api/db/cleanup` `/vacuum` | ✅ |
| WebSocket `GET /ws`（`ping`→`pong`，`subscribe`→`stats`，节流 + 变更签名去重） | ✅ 手写 RFC 6455 |

### 复核结果

`/tmp/api_review.sh` 覆盖 **70 项**，全部通过（0 失败），其中含 6 项参数校验：

- `summary?backendId=999` → 404
- `summary?backendId=abc` → 404
- `trend?start=<only>` → 400
- `trend?start>end` → 400
- `domains/proxy-stats` 缺 `domain` → 400
- `rules/domains` 缺 `rule` → 400

`go vet` / `gofmt` / `go test` 全部干净。

## Phase 3 待做

| 能力 | 说明 |
|---|---|
| GeoIP / ASN 查询 | 表已建好（`geoip_cache` / `asn_cache`），`maxmind` MMDB 本地查与在线 API 两条路都未接线 |
| WebSocket 分页与下钻推送 | `subscribe` 的 `includeDomainsPage` / `includeIPsPage` / 各 Detail 字段已解析，尚未填充进推送帧 |
| Surge 网关采集 | HTTP 轮询路径未实现；`/api/gateway/*` 的 Surge 分支返回 Clash 形态 |
| ClickHouse 双写 | 表结构已知，未实现；SQLite 已够用 |
| 实时叠加 | DTO 不叠加内存 buffer，`summary` 最新数据会晚最多一个 flush 周期（默认 30s） |

完整契约见 [`../API-DETAILS.md`](../API-DETAILS.md)（2650 行，从上游源码逐函数提取，含 40 个重写风险点）。

## 快速开始

```bash
# 从源码跑
go run ./cmd/neko \
  -seed-name home \
  -seed-url 192.168.31.1:9090 \
  -seed-token YOUR_MIHOMO_SECRET

# 编译单二进制
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o neko ./cmd/neko
```

启动后默认监听 `:3001`：

```bash
curl localhost:3001/health
curl localhost:3001/api/stats/summary
curl localhost:3001/api/stats/global
```

### Docker

```bash
docker build -t neko-master-go .
docker run -d -p 3001:3001 \
  -v $PWD/data:/app/data \
  -e SEED_BACKEND_NAME=home \
  -e SEED_BACKEND_URL=192.168.31.1:9090 \
  -e SEED_BACKEND_TOKEN=xxx \
  neko-master-go
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
| `GEOIP_LOOKUP_PROVIDER` | `online` | `online` / `local` |
| `GEOIP_ONLINE_API_URL` | `https://api.ipinfo.es/ipinfo` | 在线查 IP 归属 |
| `GEOIP_MMDB_DIR` | `./data/geoip` | 本地 MMDB 目录 |
| `COOKIE_SECRET` | — | Cookie 签名密钥 |
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
    ├── internal/
    │   ├── api/             路由层（含手写 RFC 6455 WebSocket）
    │   ├── collector/       快照 → 增量 → 缓冲
    │   ├── store/           SQLite 建表与 UPSERT
    │   ├── stats/           统计查询与按桶聚合
    │   ├── gateway/         上游 Mihomo 客户端
    │   ├── model/           共享类型
    │   └── config/
    └── web/                 （预留 embed 前端静态产物）
```

**只新增 `neko-master-go/` 目录**，不改动 `apps/` 与 `packages/`。这样上游更新时 `git merge upstream/main` 不会冲突，Go 后端按 `API-DETAILS.md` 契约补齐即可。

## 已知差异

| 项 | 上游行为 | Go 实现 | 原因 |
|---|---|---|---|
| 实时叠加 | DTO 会叠加内存 buffer | 只读已落库数据 | 待补；`summary` 会晚最多一个 flush 周期 |
| `country_stats` | 两处定义冲突 | 按 `shared` 版（含可选 `lastSeen`） | 与前端一致 |
| `rules/*/proxy-stats` | 无时间范围时恒返回 `[]` | 复刻该行为 | 源码级不一致，见 API-DETAILS §1.16 |
| `trend/aggregated` 日对齐 | 走 `strftime` 平移 | Go 侧时间平移实现 | 数值等价 |
| ClickHouse | 支持双写与查询路由 | 未实现 | SQLite 足够；后续可选 |
| Surge 网关 | 支持 HTTP 轮询 | 未实现 | Phase 3 |
| GeoIP | `maxmind` + 在线 API | 表与配置已就绪，查询未接线 | Phase 3 |
| Backend token 返回 | `/api/backends/active` 返回明文 | 全接口统一 `maskToken()` | 修上游泄露 |

## 测试

```bash
go test ./...
```

`internal/collector/tracker_test.go` 覆盖：首次快照不计量、增量做差、连接数只计一次、计数器回退、连接消失清理、零增量跳过、链路拼接、UTC 分桶。
