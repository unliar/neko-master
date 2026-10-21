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

## 前端接口核对

本节记录与 `apps/web` 前端的接口核对结果，便于后续继续核对。
核对方式：抓取前端 `lib/api.ts` 的 39 条 `buildUrl` 路径 + 4 条裸 `fetch`
（`auth/state`、`auth/verify`、`auth/logout`、`backends`），与 Go 侧
`mux.HandleFunc` 注册表逐条比对，再逐个核对查询参数与返回字段。

**结论：路径层面对齐，零缺失；参数与返回结构已修复 2 处不匹配。**

### 路径清单（39 + 4 = 43，全部有实现）

```
GET  /api/stats/summary                  GET  /api/stats/domains
GET  /api/stats/ips                      GET  /api/stats/proxies
GET  /api/stats/rules                    GET  /api/stats/countries
GET  /api/stats/devices                  GET  /api/stats/trend/aggregated
GET  /api/stats/domains/proxy-stats      GET  /api/stats/domains/ip-details
GET  /api/stats/ips/proxy-stats          GET  /api/stats/ips/domain-details
GET  /api/stats/proxies/domains          GET  /api/stats/proxies/ips
GET  /api/stats/devices/domains          GET  /api/stats/devices/ips
GET  /api/stats/rules/domains            GET  /api/stats/rules/ips
GET  /api/stats/rules/chain-flow-all     GET  /api/stats/rules/domains/proxy-stats
GET  /api/stats/rules/domains/ip-details GET  /api/stats/rules/ips/proxy-stats
GET  /api/stats/rules/ips/domain-details
GET  /api/gateway/proxies                GET  /api/gateway/rules
GET  /api/gateway/providers/proxies      GET  /api/backends/health/history
GET  /api/db/geoip                       GET  /api/db/retention
GET  /api/db/stats                       POST /api/db/cleanup
GET  /api/auth/state                     POST /api/auth/enable
POST /api/auth/disable                   POST /api/auth/verify
PUT  /api/auth/token                     POST /api/backends/test
GET  /api/backends                       POST /api/backends
PUT  /api/backends/{id}                  DELETE /api/backends/{id}
```

后端另有前端不直接调用的路由：`/api/agent/*`（agent 反向上报）、
`/api/stats/global`、`/api/stats/hourly`、`/api/stats/trend`、
`/api/stats/rule-proxy-map`、`/api/stats/rules/chain-flow`、
`/api/stats/connections`、`/api/db/vacuum`、`/api/db/geoip/lookup`、
`/api/db/geoip/status`、`/api/gateway/providers/proxies/refresh`、
`/api/backends/active`、`/api/backends/listening`、
`/api/backends/{id}/{activate,listening,clear-data,rotate-agent-token,test}`。
其中 backends 的 CRUD 子路由是前端通过路径模板拼接调用的，非缺失。

### 已核验一致（未改动）

| 项 | 状态 |
|---|---|
| 路径覆盖 | 43/43 全部有实现，零缺失 |
| `DomainRow` / `DomainStats` | 8 字段逐字段一致 |
| `IPRow` / `IPStats` | 一致；`geoIP` 由 `attachGeoIPs` 动态附加（非结构体字段） |
| `ProxyRow` / `RuleRow` / `DeviceRow` / `CountryRow` / `HourlyRow` | 字段一致 |
| `TrendPoint` / `TrafficTrendPoint` | `time` / `upload` / `download` 一致 |
| auth 四接口 | 请求体（`{token}`、`{currentToken,newToken}`）与返回结构一致 |
| `Backend` 结构 | 与前端接口一致，token 统一掩码 |

### 已修复的不匹配

| 接口 | 问题 | 修复 |
|---|---|---|
| `GET /api/gateway/rules`（Surge） | 返回 `policy`，前端 `interactive-rule-stats.tsx` 硬读 `rule.proxy`，导致 Surge 零流量规则被 `if (!proxyGroup) continue` 全部过滤 | 返回时改名 `proxy` 并固定 `size: 0`，补 `_source: 'surge'`（对齐上游 `app.ts`） |
| `GET /api/backends/health/history` | 返回单个对象且读错参数；前端要 `BackendHealthHistory[]` 数组并传 `from`/`to` | 改为按 backend 分组的数组，读 `from`/`to`/`backendId`，缺省近 24h，`backendId` 解析失败返回 400 |

### 风险点 / 待继续核对

1. **`geoIP` 字段来源不一致**：前端 `IPStats` 声明了 `geoIP`，但 Go 侧
   `IPRow` 结构体没有该字段，靠 `attachGeoIPs` 序列化后动态附加。
   功能正常（响应里确实有），但类型层面不对齐。若要统一需把 geoIP
   收进结构体字段。
2. **`/stats/*` 的时间范围参数未逐一核对**：前端普遍传 `start`/`end`，
   Go 侧各 handler 是否都正确消费、缺省行为是否与上游一致，尚未逐个验证。
3. **`limit` / `offset` / `sortBy` / `sortOrder` / `search` 的边界值**
   （空串、非法值、超范围）未与上游逐一比对，可能返回不同的错误码。
4. **错误响应体格式**：Go 侧统一 `{"error": "..."}`，但上游部分路由
   返回 `{"error":..., "message":..., "statusCode":...}`（Fastify 形态），
   前端 `isApiStatus` 只看状态码，故暂不影响，但严格来说未对齐。
5. **`_source` 字段**：`agent-cache` / `surge` / `clash` 三种取值，
   目前仅 Surge 分支已输出，Clash 与 agent 分支未加，前端类型里是可选字段。
6. **未做端到端联调**：以上均为静态代码比对，未启动服务用真实请求验证。
   建议下一步起服务跑一遍前端页面的核心接口（summary / domains / rules / gateway）。
7. **`/api/stats/rules/chain-flow-all`** 前端传 `start`/`end`，Go 侧
   是否按时间范围过滤尚未核对。


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
