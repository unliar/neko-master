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

## Phase 3（已完成）

| 能力 | 实现 | 对齐上游 |
|---|---|---|
| GeoIP / ASN 查询 | `internal/geo`（纯 Go MMDB 解析器 + 在线 API 回落） | `geoip_cache` / `asn_cache` 双层缓存 |
| WebSocket 分页与下钻推送 | `includeDomainsPage` / `includeIPsPage` / 各 Detail 字段已填充推送帧 | `websocket.types.ts` |
| Surge 网关采集 | `internal/gateway/surge.go` + `internal/collector/surge_tracker.go`：轮询 `/v1/requests/recent`，`x-key` 鉴权，策略链从 notes 解析 | `surge.collector.ts`（933 行） |
| Surge 网关 API | `/api/gateway/proxies` 走 `/v1/policies/{name}`；`/providers/proxies` 走 `/v1/policy_groups/select`（风险点 27，两路由不合并）；`rules` 走 `parseSurgeRule`（`FINAL`→`MATCH`） | `gateway-utils.ts` |
| ClickHouse 双写 | `internal/clickhouse`：SQLite 为主 + CH 旁路，写入统一走 `*_buffer` 表，`CH_ENABLED != 1` 时完全 no-op | `clickhouse.config.ts` / `clickhouse.writer.ts` |
| 实时叠加 | `internal/realtime`：逐函数对齐上游 `realtime-store.ts`（`recordTraffic` / `getSummaryDelta` / `getTodayDelta` / `clearTraffic*` / `pruneOldBuckets`） | summary 为累加器，仅在 flush 成功后清零 |

### 配置变量

```bash
# ClickHouse 双写（默认关闭）
CH_ENABLED=1              # 只有 1 才启用
CH_HOST=clickhouse
CH_PORT=8123
CH_DATABASE=neko_master
CH_USER=default
CH_PASSWORD=
CH_REQUIRED=1             # 1 = 连不上就启动失败；0 = 告警后降级为禁用
CH_AUTO_CREATE_TABLES=1   # 0 = 跳过建表
CH_UNHEALTHY_THRESHOLD=5  # 连续失败次数，超阈自动关闭双写
CH_WRITE_MAX_PENDING_ROWS=200000
```

### Surge backend 注意事项

- **`remoteAddress` 不是真实落地 IP**。Surge 的 DNS 解析发生在代理服务器侧，因此走代理时
  `remoteHost` 是目标域名、`remoteAddress` 只能拿到代理节点 IP。直连（`remoteHost`
  本身就是 IP）时才能得到真实 IP。
- **`inBytes` / `outBytes` 方向与 Clash 相反**：`in` 是下载，`out` 是上传。
- **完成的请求可能在后续轮询中重复出现**，因此用 `recentlyCompleted`
  记录最终字节值（TTL 5 分钟）防重复计数。

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
| 实时叠加 | DTO 会叠加内存 buffer | 已实现且逐函数对齐；flush 失败时保留以供重试，成功才 `clearTraffic` | 上游维拄 8 个维度 Map，Go 侧仅实现 summary/minute 两个（维度 Map Summary 不读） |
| `country_stats` | 两处定义冲突 | 按 `shared` 版（含可选 `lastSeen`） | 与前端一致 |
| `rules/*/proxy-stats` | 无时间范围时恒返回 `[]` | 复刻该行为 | 源码级不一致，见 API-DETAILS §1.16 |
| `trend/aggregated` 日对齐 | 走 `strftime` 平移 | Go 侧时间平移实现 | 数值等价 |
| ClickHouse | 支持双写与查询路由 | 已实现双写（`CH_ENABLED=1`）；读路由未接线 | SQLite 仍是唯一权威源；CH 失败不影响主流程 |
| Surge 网关 | 支持 HTTP 轮询 | 已实现（`/v1/requests/recent` + `/v1/policies*` + `/v1/rules`） | 上游不提供真实落地 IP，属先天限制 |
| GeoIP | `maxmind` + 在线 API | 已接线 | 纯 Go MMDB 解析，无 CGO |
| Backend token 返回 | `/api/backends/active` 返回明文 | 全接口统一 `maskToken()` | 修上游泄露 |

## 测试

```bash
go test ./...
```

`internal/collector/tracker_test.go` 覆盖（Clash）：首次快照不计量、增量做差、连接数只计一次、计数器回退、连接消失清理、零增量跳过、链路拼接、UTC 分桶。

`internal/collector/surge_tracker_test.go` 覆盖（Surge）：`startDate` 缺失时首次带字节即计量、preexisting 只建基线不计量、`in/out` 方向映射、连接数只计一次、计数器回退、完成请求防重复计数（无新字节跳过 / 有新字节重新处理）、notes 策略链反转、过期清理、`sourceIP` 传播。

`internal/realtime/realtime_test.go` 覆盖：同分钟同键合并、`Prune` 仅剪旧桶不动未来桶、`Since` 时间过滤、`Overlay` 就地累加 vs 追加新行、不污染入参、多维键隔离、Manager 按 backend 隔离，以及关键的**“落盘后不重复计数”**一致性场景。

`internal/clickhouse/clickhouse_test.go` 覆盖：JSONEachRow 类型精确性（UInt32/UInt64 必须是数字）、引号与控制字符转义、country/agg 列对齐、多行分隔、负值与溢出 clamp、同分钟桶预聚合并、默认禁用。

`internal/gateway/surge_test.go` 覆盖：`parseSurgeRule` 全分支（含 `FINAL`→`MATCH` 类型改写、引号抹除、两段兜底）、策略路径反转与单段忽略、`chains` 兜底与 `DIRECT` 默认、`surgeSourceIP` 回环兜底、`extractHost` 含 IPv6 括号形态、`ResolveTarget` 域名/代理 IP/直连 IP 三种分支。
