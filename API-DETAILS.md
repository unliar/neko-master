# neko-master 后端 HTTP API 规格（Go 重写参考）

> 本文档严格从源码提取，未做任何猜测。凡源码未明确之处，均标注 **「源码中未明确」**。
> 所有 TypeScript 接口均为**原样复制**，未改写。
>
> 源码根：`/Users/flyneko/flyneko-space/neko-master`
> 主要依据：
> - `apps/collector/src/modules/stats/stats.controller.ts`（790 行）
> - `apps/collector/src/modules/stats/stats.service.ts`（1566 行）
> - `apps/collector/src/modules/stats/stats.types.ts`（135 行）
> - `apps/collector/src/modules/app/app.ts`（1446 行）
> - `apps/collector/src/modules/auth/*`
> - `apps/collector/src/modules/websocket/*`
> - `apps/collector/src/modules/collector/*`
> - `apps/collector/src/database/schema.ts`（完整的 SQLite DDL）
> - `apps/collector/src/database/repositories/*`
> - `packages/shared/src/index.ts`、`gateway-utils.ts`、`geo-ip-utils.ts`

---

## 0. 总览与全局约定

### 0.1 服务与技术栈

| 项 | 值 | 出处 |
|---|---|---|
| HTTP 框架 | Fastify（`bodyLimit: 5 * 1024 * 1024`，即 5MB） | `app.ts` |
| 监听地址 | `0.0.0.0`，端口由 `AppOptions.port` 传入 | `app.ts` |
| Cookie 插件 | `@fastify/cookie`，`secret = process.env.COOKIE_SECRET \|\| db.getOrCreateCookieSecret()` | `app.ts` |
| CORS | `CORS_ORIGIN` 逗号分隔白名单；**未设置时 `origin: false`**（拒绝跨域），`credentials` 仅在设置了白名单时为 `true` | `app.ts` |
| 全局响应头 | `X-Content-Type-Options: nosniff`、`X-Frame-Options: DENY`、`Referrer-Policy: strict-origin-when-cross-origin` | `app.ts` `onSend` |
| 请求体解压 | `preParsing` 钩子：`content-encoding: gzip` 时 gunzip 并**删除 `content-length`** | `app.ts` |

### 0.2 路由注册总表

```
app.get   /health
app.post  /api/agent/heartbeat
app.post  /api/agent/report
app.post  /api/agent/config
app.post  /api/agent/policy-state
app.get   /api/gateway/proxies
app.get   /api/gateway/providers/proxies
app.post  /api/gateway/providers/proxies/refresh
app.get   /api/gateway/rules
register  backendController -> prefix /api/backends
register  statsController   -> prefix /api/stats
register  authController    -> prefix /api/auth
register  configController  -> prefix /api/db
```

`/api/stats` 下共 **29** 条路由，`/api/auth` 下 **6** 条，`/api/backends` 下 **13** 条，`/api/db` 下 **7** 条，`/api/gateway` 下 **4** 条，`/api/agent` 下 **4** 条，另有 `/health` **1** 条。

**合计 64 条 HTTP 路由**（不含 WebSocket）。各模块的精确计数来自对源码的 `fastify.get/post/put/delete` 与 `app.get/post/put` 出现次数统计。

### 0.3 三个所有 /api/stats/* 接口共用的解析规则

这三段逻辑在 `stats.controller.ts` 中定义，本文件所有 stats 接口都复用，后续不再重复展开。

#### (A) `backendId` 解析 —— `getBackendId()` → `service.resolveBackendId(query.backendId)`

```ts
resolveBackendId(rawBackendId?: string): number | null {
  if (rawBackendId) {
    const id = Number.parseInt(rawBackendId, 10);
    return Number.isNaN(id) ? null : id;
  }
  return this.db.getActiveBackend()?.id ?? null;
}
```
- 传了 `backendId` 但解析不出整数 → 返回 `null` → 接口返回 **404 `{"error":"No backend specified or active"}`**。
- 未传 → 回退到「当前激活 backend」；没有激活 backend 同样 404。
- ⚠️ 注意：`parseInt` 是**宽松**的，`"3abc"` 会得到 `3`。

#### (B) 时间范围解析 —— `getTimeRange()`

非 showcase 模式：
- `start`、`end` 都不传 → `{ active: false }`（此时走「预聚合快照表」路径，见 §6.4）。
- 只传其一 → **400 `{"error":"Both start and end must be provided together"}`**。
- 任一无法被 `new Date()` 解析 → **400 `{"error":"Invalid time range format, expected ISO datetime"}`**。
- `start > end` → **400 `{"error":"start must be less than or equal to end"}`**。
- 正常 → `{ start: ISO, end: ISO, active: true }`。

showcase 模式（`SHOWCASE_SITE_MODE=true`）：
- 不校验错误，**永远不会 400**。
- 不传 `start` → 取 `[now-24h, now]`。
- `start` 无法解析 → 取 `[now-24h, now]`；`end` 无法解析 → 取 `now`。
- `start < now-24h` → 被 **clamp 到 `now-24h`**。
- `start > end` → 重置为 `[now-24h, now]`。
- 恒返回 `active: true`。

#### (C) `limit` 解析 —— `service.parseLimit(raw, fallback, max)`

```ts
parseLimit(raw: string | undefined, fallback: number, max: number): number {
  if (raw === undefined || raw === null || raw === '') return fallback;
  const parsed = parseInt(raw, 10);
  if (Number.isNaN(parsed) || parsed <= 0) return fallback;
  return Math.min(parsed, max);
}
```
即：**缺省/空串/非数字/≤0 → fallback；超过 max → 截断到 max**。注意 `offset` 用的是另一个函数 `parseOffset`（缺省 0，负数按 0）。

### 0.4 双层数据源（SQLite / ClickHouse）对 API 语义的影响

`StatsService` 每个方法都有 `xxx()`（纯 SQLite）和 `xxxWithRouting()`（走 ClickHouse，失败回退 SQLite）两个版本。控制器**全部调用 `WithRouting` 版本**。

```ts
shouldUseForRange(start?, end?): boolean {
  if (!this.shouldUse()) return false;      // CH 未启用 或 STATS_QUERY_SOURCE === 'sqlite'
  if (!start || !end) return false;         // 必须同时有 start/end
  const startMs = new Date(start).getTime();
  const endMs = new Date(end).getTime();
  return Number.isFinite(startMs) && Number.isFinite(endMs) && startMs <= endMs;
}
```

- ClickHouse 路径**只在显式传了 `start` 且 `end` 时生效**。不传时间范围时永远走 SQLite 预聚合表。
- CH 相关环境变量：`STATS_QUERY_SOURCE`（值为 `sqlite` 时强制走 SQLite）、`CH_STRICT_STATS=1`（禁止 SQLite 回退，直接抛错 500）。
- 对 Go 重写而言：**外部可观察的响应结构由 `shared` 的类型决定，与数据源无关**；但「不传时间范围」与「传了时间范围」在数值上有语义差异（见 §6.4）。

---

## 1. `/api/stats/*` 接口清单（29 个）

### 1.1 路由一览

| # | 方法 | 路径 | 必填 query | 响应类型 |
|---|---|---|---|---|
| 1 | GET | `/api/stats/summary` | — | `SummaryResponse` |
| 2 | GET | `/api/stats/global` | — | `GlobalSummary` |
| 3 | GET | `/api/stats/domains` | — | `PaginatedDomainStats` |
| 4 | GET | `/api/stats/ips` | — | `PaginatedIPStats` |
| 5 | GET | `/api/stats/domains/proxy-stats` | `domain` | `ProxyTrafficStats[]` |
| 6 | GET | `/api/stats/domains/ip-details` | `domain` | `IPStats[]` |
| 7 | GET | `/api/stats/ips/proxy-stats` | `ip` | `ProxyTrafficStats[]` |
| 8 | GET | `/api/stats/ips/domain-details` | `ip` | `DomainStats[]` |
| 9 | GET | `/api/stats/proxies/domains` | `chain` | `DomainStats[]` |
| 10 | GET | `/api/stats/proxies/ips` | `chain` | `IPStats[]` |
| 11 | GET | `/api/stats/proxies` | — | `ProxyStats[]` |
| 12 | GET | `/api/stats/rules` | — | `RuleStats[]` |
| 13 | GET | `/api/stats/rules/domains` | `rule` | `DomainStats[]` |
| 14 | GET | `/api/stats/rules/ips` | `rule` | `IPStats[]` |
| 15 | GET | `/api/stats/rules/domains/proxy-stats` | `rule`,`domain` | `ProxyTrafficStats[]` |
| 16 | GET | `/api/stats/rules/domains/ip-details` | `rule`,`domain` | `IPStats[]` |
| 17 | GET | `/api/stats/rules/ips/proxy-stats` | `rule`,`ip` | `ProxyTrafficStats[]` |
| 18 | GET | `/api/stats/rules/ips/domain-details` | `rule`,`ip` | `DomainStats[]` |
| 19 | GET | `/api/stats/rules/chain-flow` | `rule` | 匿名结构（见 §1.19） |
| 20 | GET | `/api/stats/rules/chain-flow-all` | — | `RuleChainFlowAll` |
| 21 | GET | `/api/stats/rule-proxy-map` | — | `{rule, proxies}[]` |
| 22 | GET | `/api/stats/countries` | — | `CountryStats[]` |
| 23 | GET | `/api/stats/devices` | — | `DeviceStats[]` |
| 24 | GET | `/api/stats/devices/domains` | —（`sourceIP` 缺失返回 `[]`） | `DomainStats[]` |
| 25 | GET | `/api/stats/devices/ips` | —（同上） | `IPStats[]` |
| 26 | GET | `/api/stats/hourly` | — | `HourlyStats[]` |
| 27 | GET | `/api/stats/trend` | — | `TrafficTrendPoint[]` |
| 28 | GET | `/api/stats/trend/aggregated` | — | `TrafficTrendPoint[]` |
| 29 | GET | `/api/stats/connections` | — | 实际恒为 `[]`（见 §1.29） |

> 本节清单共 **29 条**路由，与 `stats.controller.ts` 中 `fastify.get(...)` 的出现次数（29）完全一致。

#### 全局错误汇总（适用于所有 stats 接口）

| 条件 | 状态码 | Body |
|---|---|---|
| `backendId` 无法解析 / 无激活 backend | 404 | `{"error":"No backend specified or active"}` |
| `summary` 且 backend 不存在 | 404 | `{"error":"Backend not found"}` |
| 时间范围成对/格式/顺序错误（仅非 showcase） | 400 | 见 §0.3(B) |
| 必填业务参数缺失 | 400 | 见各接口 |

---

### 1.2 `GET /api/stats/summary`

**说明**：仪表盘首页的汇总卡片。返回单个 backend 在给定时间范围内的总流量、总连接数、独立域名数、独立 IP 数、Top10 域名、Top10 IP、全部 proxy 维度统计、全部 rule 维度统计、最近 24 小时的分时流量，以及 backend 自身的元信息与在线状态。当时间范围「贴近当前时间」时，会叠加内存中的实时增量（尚未落库的部分）。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `backendId` | string（int） | 否 | 当前激活 backend | 见 §0.3(A) |
| `start` | string（ISO datetime） | 与 `end` 成对 | — | 见 §0.3(B) |
| `end` | string（ISO datetime） | 与 `start` 成对 | — | 见 §0.3(B) |

**响应 TypeScript 定义**（`stats.types.ts`，原样）：

```ts
export interface SummaryResponse {
  backend: {
    id: number;
    name: string;
    isActive: boolean;
    listening: boolean;
  };
  totalConnections: number;
  totalUpload: number;
  totalDownload: number;
  totalDomains: number;
  totalIPs: number;
  totalRules: number;
  totalProxies: number;
  todayUpload: number;
  todayDownload: number;
  topDomains: DomainStats[];
  topIPs: IPStats[];
  proxyStats: ProxyStats[];
  ruleStats: RuleStats[];
  hourlyStats: HourlyStats[];
}
```

**字段填充规则（源码 `getSummary` / `getSummaryWithRouting`）**

| 字段 | 来源 |
|---|---|
| `backend.id/name/isActive/listening` | `backend_configs` 表（`is_active`、`listening` 为 0/1） |
| `totalConnections/totalUpload/totalDownload` | `getSummary()` 聚合，再叠加 `realtimeStore.applySummaryDelta` |
| `totalDomains` | **注意**：取自 `summary.uniqueDomains`（**不是** `summaryWithRealtime`） |
| `totalIPs` | 同上，取自 `summary.uniqueIPs` |
| `totalRules` | `ruleStats.length`（不是单独 count 查询） |
| `totalProxies` | `proxyStats.length` |
| `todayUpload/todayDownload` | `getTrafficInRange(...)` + `realtimeStore.getTodayDelta(...)` |
| `topDomains` | 固定 **Top 10**（`getTopDomainsLight(backendId, 10, ...)`） |
| `topIPs` | 固定 **Top 10**，CH 路径会经 `populateGeoIPs` 补 `geoIP`/`asn` |
| `proxyStats` | 全量，按流量降序（见 §6.5） |
| `ruleStats` | 全量 |
| `hourlyStats` | `getHourlyStats(backendId, 24, start, end)` |

**Top10/TopIP 等元素类型**（`packages/shared/src/index.ts`，原样）：

```ts
export interface DomainStats {
  domain: string;
  ips: string[];
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
  rules: string[];
  chains: string[];
}

export interface IPStats {
  ip: string;
  domains: string[];
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
  asn?: string;
  geoIP?: GeoIPInfo;
  chains?: string[];
}

export interface ProxyStats {
  chain: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}

export interface RuleStats {
  rule: string;
  finalProxy: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}

export interface HourlyStats {
  hour: string;
  upload: number;
  download: number;
  connections: number;
}

export interface DeviceStats {
  sourceIP: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}

export interface CountryStats {
  country: string;
  countryName: string;
  continent: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen?: string;
}
```

```ts
// packages/shared/src/geo-ip-utils.ts
export interface GeoIPInfo {
  countryCode: string;
  countryName: string;
  city: string;
  asOrganization: string;
}
```

---

### 1.3 `GET /api/stats/global`

**说明**：跨**所有** backend 的全局汇总。不接收任何 query（`getBackendId` / `getTimeRange` 都不调用），也**不受 `start`/`end` 影响** —— 控制器签名就是 `fastify.get('/global', async () => {...})`。

**Query 参数**：无。

**响应 TypeScript 定义**（`stats.types.ts`，原样）：

```ts
export interface GlobalSummary {
  totalConnections: number;
  totalUpload: number;
  totalDownload: number;
  uniqueDomains: number;
  uniqueIPs: number;
  backendCount: number;
}
```

**说明补充**：`backendCount` 在 CH 路径下取自 `db.getAllBackends().length`；SQLite 路径下由 `db.getGlobalSummary()` 内部计算。

---

### 1.4 `GET /api/stats/domains`

**说明**：某 backend 的域名维度统计，**分页**，支持搜索与多字段排序。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 取值/范围 |
|---|---|---|---|---|
| `backendId` | string（int） | 否 | 激活 backend | §0.3(A) |
| `start` / `end` | string（ISO） | 成对 | — | §0.3(B) |
| `offset` | string（int） | 否 | `0` | 负数或 NaN → 0 |
| `limit` | string（int） | 否 | `50` | 上限 **200** |
| `sortBy` | string | 否 | `totalDownload` | 白名单：`domain` \| `totalDownload` \| `totalUpload` \| `totalTraffic` \| `totalConnections` \| `lastSeen`；不在白名单 → 回退 `totalDownload` |
| `sortOrder` | string | 否 | `desc` | 仅 `'asc'`（大小写敏感）走升序，**其他任何值都按 DESC** |
| `search` | string | 否 | — | 走 `domain LIKE '%<search>%'` |

**响应 TypeScript 定义**（`stats.types.ts`，原样）：

```ts
export interface PaginatedDomainStats {
  data: DomainStats[];
  total: number;
}
```

**说明**：`data` 为当前页的 `DomainStats[]`；`total` 是**满足时间范围 + search 的域名总数**（`SELECT COUNT(*) FROM (SELECT domain ... GROUP BY domain)`），用于前端计算页数。当 `shouldIncludeRealtime` 为真时，会在合并实时增量后**重新做排序与分页**（`realtimeStore.mergeDomainStatsPaginated`）。

---

### 1.5 `GET /api/stats/ips`

**说明**：某 backend 的 IP 维度统计，分页 + 搜索 + 排序。参数语义与 §1.4 完全一致（`sortBy` 白名单里把 `domain` 换成 `ip`）。

**Query 参数**：同 §1.4，`sortBy` 白名单为 `ip` \| `totalDownload` \| `totalUpload` \| `totalTraffic` \| `totalConnections` \| `lastSeen`。

**响应 TypeScript 定义**（`stats.types.ts`，原样）：

```ts
export interface PaginatedIPStats {
  data: IPStats[];
  total: number;
}
```

**说明**：CH 路径下 `data` 会经 `populateGeoIPs()` 从 `geoip_cache` 表补齐 `geoIP`（`{countryCode, countryName, city, asOrganization}`）与 `asn`。

---

### 1.6 `GET /api/stats/domains/proxy-stats`

**说明**：给定域名，返回其在**各条代理链（chain）**上的流量拆分，用于「这个域名走了哪些出口」。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `domain` | string | **是** | — | 缺失 → **400 `{"error":"Domain parameter is required"}`** |
| `sourceIP` | string | 否 | — | 过滤设备 |
| `sourceChain` | string | 否 | — | 过滤出口链 |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**（`packages/shared/src/index.ts`，原样）：

```ts
// Per-proxy traffic breakdown for a specific domain or IP
export interface ProxyTrafficStats {
  chain: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
}
```

> ⚠️ **源码不一致**：`stats.service.ts` 中该方法的返回类型标注为 `any[]` / `Promise<any[]>`，而底层 repository 方法 `getDomainProxyStats(...)` 的签名是：
> `Array<{ chain: string; totalUpload: number; totalDownload: number; totalConnections: number }>` —— 结构等同 `ProxyTrafficStats`，只是未命名为该接口。SQL 为 `GROUP BY chain ORDER BY (SUM(upload)+SUM(download)) DESC`。

---

### 1.7 `GET /api/stats/domains/ip-details`

**说明**：给定域名，返回其解析到/连接过的 **IP 列表**及各自流量。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `domain` | string | **是** | — | 缺失 → 400 `{"error":"Domain parameter is required"}` |
| `sourceIP` | string | 否 | — | |
| `sourceChain` | string | 否 | — | |
| `limit` | string（int） | 否 | `100` | 上限 **2000** |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**：`IPStats[]`（见 §1.2），CH 路径补齐 `geoIP`/`asn`。

---

### 1.8 `GET /api/stats/ips/proxy-stats`

**说明**：给定 IP，返回其在各代理链上的流量拆分。与 §1.6 对称。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `ip` | string | **是** | — | 缺失 → **400 `{"error":"IP parameter is required"}`** |
| `sourceIP` | string | 否 | — | |
| `sourceChain` | string | 否 | — | |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**：同 §1.6（`ProxyTrafficStats[]` 结构，源码标注 `any[]`）。

---

### 1.9 `GET /api/stats/ips/domain-details`

**说明**：给定 IP，返回与之相关的**域名列表**及各自流量。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `ip` | string | **是** | — | 缺失 → 400 `{"error":"IP parameter is required"}` |
| `sourceIP` | string | 否 | — | |
| `sourceChain` | string | 否 | — | |
| `limit` | string（int） | 否 | `100` | 上限 **2000** |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**：`DomainStats[]`（见 §1.2）。

---

### 1.10 `GET /api/stats/proxies/domains`

**说明**：给定一条代理链（`chain`），返回经过该链的**域名**排行。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `chain` | string | **是** | — | 缺失 → **400 `{"error":"Chain parameter is required"}`** |
| `limit` | string（int） | 否 | `5000` | 上限 **20000** |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**：`DomainStats[]`。SQL：`WHERE chain = ? AND domain != '' GROUP BY domain ORDER BY (SUM(upload)+SUM(download)) DESC LIMIT ?`。

---

### 1.11 `GET /api/stats/proxies/ips`

**说明**：给定代理链，返回经过该链的 **IP** 排行。参数与 §1.10 完全一致。

**Query 参数**：`chain`（必填，400 同上）、`limit`（默认 5000，上限 20000）、`backendId` / `start` / `end`。

**响应 TypeScript 定义**：`IPStats[]`（CH 路径补齐 geo）。

---

### 1.12 `GET /api/stats/proxies`

**说明**：该 backend 的**全部代理链维度汇总**，用于「代理排行 / 出口占比」图表。

**Query 参数**：仅 `backendId` / `start` / `end`（§0.3）。**无 `limit`**。

**响应 TypeScript 定义**（`stats.types.ts` 经 `export type { ... ProxyStats ... }` 转出，实体定义在 shared）：

```ts
export interface ProxyStats {
  chain: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}
```
响应形态：`ProxyStats[]`。

---

### 1.13 `GET /api/stats/rules`

**说明**：该 backend 的**全部规则维度汇总**。规则名（`rule`）的归一化逻辑见 §6.6（多跳链路时取最后一跳 policy group 名）。

**Query 参数**：仅 `backendId` / `start` / `end`。无 `limit`。

**响应 TypeScript 定义**：`RuleStats[]`

```ts
export interface RuleStats {
  rule: string;
  finalProxy: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}
```

**两种数据源差异（重要）**
- 无时间范围（走 `rule_stats` 表）：`SELECT rule, final_proxy as finalProxy, ...`。
- 有时间范围（走 `minute_dim_stats` / `hourly_dim_stats`）：`SELECT rule, MAX(chain) as finalProxy, SUM(upload)... GROUP BY rule ORDER BY (SUM(upload)+SUM(download)) DESC`。**`finalProxy` 变成 `MAX(chain)`**，语义与表里存的 `final_proxy` 并不完全等价。

---

### 1.14 `GET /api/stats/rules/domains`

**说明**：给定规则名，返回命中该规则的**域名**排行。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `rule` | string | **是** | — | 缺失 → **400 `{"error":"Rule parameter is required"}`** |
| `limit` | string（int） | 否 | `5000` | 上限 **20000** |
| `backendId` / `start` / `end` | — | — | — | §0.3 |

**响应 TypeScript 定义**：`DomainStats[]`。SQL 里 `rules` 字段被硬编码为 `[rule]`（单元素数组），`chains` 经 `expandShortChainsForRules` 补全为完整链路。

---

### 1.15 `GET /api/stats/rules/ips`

**说明**：给定规则名，返回命中该规则的 **IP** 排行。参数与 §1.14 完全一致。

**Query 参数**：`rule`（必填，400 同上）、`limit`（默认 5000，上限 20000）、`backendId` / `start` / `end`。

**响应 TypeScript 定义**：`IPStats[]`。

---

### 1.16 `GET /api/stats/rules/domains/proxy-stats`

**说明**：规则 × 域名 维度下，再按代理链拆分。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 |
|---|---|---|---|
| `rule` | string | **是** | — |
| `domain` | string | **是** | — |
| `backendId` / `start` / `end` | — | 否 | — |

二者任一缺失 → **400 `{"error":"Rule and domain parameters are required"}`**。

**响应 TypeScript 定义**：同 §1.6（`ProxyTrafficStats[]` 结构，源码标注 `any[]`）。SQL：`WHERE rule = ? AND domain = ? GROUP BY chain ORDER BY (SUM(upload)+SUM(download)) DESC`。

> ⚠️ **行为差异（重要）**：SQLite 实现 `getRuleDomainProxyStats` 在**无时间范围**（`parseMinuteRange` 返回 null）时**直接 `return []`**，不做任何查询。即：不带 `start`/`end` 调本接口恒返回空数组。

---

### 1.17 `GET /api/stats/rules/domains/ip-details`

**说明**：规则 × 域名 维度下的 IP 明细。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `rule` | string | **是** | — | |
| `domain` | string | **是** | — | |
| `limit` | string（int） | 否 | `100` | 上限 **2000** |
| `backendId` / `start` / `end` | — | 否 | — | |

缺 `rule` 或 `domain` → **400 `{"error":"Rule and domain parameters are required"}`**。

**响应 TypeScript 定义**：`IPStats[]`。

---

### 1.18 `GET /api/stats/rules/ips/proxy-stats`

**说明**：规则 × IP 维度下按代理链拆分。

**Query 参数**

| 名称 | 类型 | 必填 |
|---|---|---|
| `rule` | string | **是** |
| `ip` | string | **是** |
| `backendId` / `start` / `end` | — | 否 |

任一缺失 → **400 `{"error":"Rule and IP parameters are required"}`**。

**响应 TypeScript 定义**：同 §1.6。同样受「无时间范围返回 `[]`」影响（`getRuleIPProxyStats` 实现与 §1.16 相同）。

---

### 1.19 `GET /api/stats/rules/ips/domain-details`

**说明**：规则 × IP 维度下的域名明细。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `rule` | string | **是** | — | |
| `ip` | string | **是** | — | |
| `limit` | string（int） | 否 | `100` | 上限 **2000** |
| `backendId` / `start` / `end` | — | 否 | — |

缺 `rule` 或 `ip` → **400 `{"error":"Rule and IP parameters are required"}`**。

**响应 TypeScript 定义**：`DomainStats[]`。

---

### 1.20 `GET /api/stats/rules/chain-flow`

**说明**：给定一条规则，返回其**流量链路 DAG**（规则 → 策略组 → 代理 的节点与边），用于「链路流程图」。

**Query 参数**

| 名称 | 类型 | 必填 |
|---|---|---|
| `rule` | string | **是**（缺失 → **400 `{"error":"Rule parameter is required"}`**） |
| `backendId` / `start` / `end` | — | 否 |

**响应 TypeScript 定义**（源码 `rule.repository.ts` 的**内联返回类型**，无命名接口）：

```ts
{
  nodes: Array<{ name: string; totalUpload: number; totalDownload: number; totalConnections: number }>;
  links: Array<{ source: number; target: number }>;
}
```

**说明**：
- `nodes[i].name` 是链路里的一个跳（规则名 / 策略组名 / 代理名）。
- `links` 用**节点数组下标**表示边（`source` → `target`）。
- 节点流量 = 经过该节点的所有 chain 的流量累加。
- `flowPath.length < 2` 的链路被丢弃（即不产生单节点图）。
- 无时间范围时读 `rule_chain_traffic` 表；有时间范围时读 `minute_dim_stats`/`hourly_dim_stats` 并用 `rule_chain_traffic` 做基线 remap（`remapRangeRowsToFullChains`），保证短链补齐为完整链路。
- 调用时额外传入 `proxyConfig`（来自 agent 模式下 `realtimeStore.getAgentConfig(backendId)?.proxies`）用于 enrich 短链。

---

### 1.21 `GET /api/stats/rules/chain-flow-all`

**说明**：把**所有规则**的链路合并成一张统一的 DAG，并给出每条规则的路径索引。

**Query 参数**：仅 `backendId` / `start` / `end`。

**响应 TypeScript 定义**（`packages/shared/src/index.ts`，原样）：

```ts
export interface RuleChainFlowAll {
  nodes: Array<{
    name: string;
    layer: number;
    nodeType: "rule" | "group" | "proxy";
    totalUpload: number;
    totalDownload: number;
    totalConnections: number;
    rules: string[];
  }>;
  links: Array<{
    source: number;
    target: number;
    rules: string[];
  }>;
  rulePaths: Record<string, { nodeIndices: number[]; linkIndices: number[] }>;
  maxLayer: number;
}
```

**说明补充**：
- `nodes[].layer` 是分层序号，`maxLayer` 是最大层号（用于前端横向布局）。
- `nodes[].nodeType` 区分 `rule`/`group`/`proxy`。
- `nodes[].rules` / `links[].rules` 记录该节点/边被哪些规则经过。
- `rulePaths[ruleName]` 给出该规则经过的节点下标与边下标。
- **行数上限**：`RULE_CHAIN_FLOW_MAX_ROWS`（默认 `2000`，最小 100），SQL 里 `ORDER BY (SUM(upload)+SUM(download)) DESC LIMIT ?` —— 即只取流量最高的前 N 条 rule×chain 组合建图。

---

### 1.22 `GET /api/stats/rule-proxy-map`

**说明**：规则 → 代理的映射表。**不接收 `start`/`end`**（控制器里只调 `getBackendId`，不调 `getTimeRange`）。

**Query 参数**：仅 `backendId`。

**响应 TypeScript 定义**（源码 `rule.repository.ts` 内联）：直接是数组，**不是** `Record`：

```ts
Array<{ rule: string; proxies: string[] }>
```

**说明**：注意与 `packages/shared` 中同名的 `RuleProxyMapping`（`{rule, proxy, totalUpload, totalDownload, totalConnections}`）**不是同一个东西** —— 本接口返回的是 `{rule, proxies[]}`，由 `rule_proxy_map` 表 `SELECT rule, proxy ... ORDER BY rule, proxy` 后在 JS 里聚合成数组。

---

### 1.23 `GET /api/stats/countries`

**说明**：按国家/地区维度聚合的流量统计，供世界地图/国家榜使用。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `limit` | string（int） | 否 | `50` | 上限 **2000** |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

**响应 TypeScript 定义**（`stats.types.ts` 也有同名定义，但**无 `lastSeen`**）：

`stats.types.ts` 版本：
```ts
export interface CountryStats {
  country: string;
  countryName: string;
  continent: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
}
```

`packages/shared/src/index.ts` 版本（多一个可选 `lastSeen`）：
```ts
export interface CountryStats {
  country: string;
  countryName: string;
  continent: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen?: string;
}
```

> ⚠️ **两个同名接口不一致**，`stats.service.ts` 用的 import 是 `stats.types.ts` 的版本；`country.repository.ts` 从 shared 导入。Go 实现建议按 shared 版本（含可选 `lastSeen`）输出，以兼容可能带 `lastSeen` 的行。

**说明**：无时间范围时读 `country_stats` 表；有时间范围时优先读 `minute_country_stats`，若为空则「回退 1：从 dim_stats + geoip_cache 派生」，源码中还有进一步的 fallback（`country.repository.ts` 内）。

---

### 1.24 `GET /api/stats/devices`

**说明**：按设备（源 IP，`source_ip`）维度的流量统计。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `limit` | string（int） | 否 | `50` | 上限 **2000** |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

**响应 TypeScript 定义**：`DeviceStats[]`

```ts
export interface DeviceStats {
  sourceIP: string;
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  lastSeen: string;
}
```

---

### 1.25 `GET /api/stats/devices/domains`

**说明**：给定设备（`sourceIP`），返回该设备访问过的域名排行。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `sourceIP` | string | **否**（但缺失/空 → 直接返回 `[]`，不报错） | `''` | |
| `limit` | string（int） | 否 | `5000` | 上限 **20000** |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

**响应 TypeScript 定义**：`DomainStats[]`。

**说明**：`service.getDeviceDomainsWithRouting` 第一行即 `if (!sourceIP) return [];`。

---

### 1.26 `GET /api/stats/devices/ips`

**说明**：给定设备，返回该设备访问过的 IP 排行。参数与 §1.25 一致，`sourceIP` 缺失同样返回 `[]`。

**Query 参数**：`sourceIP`、`limit`（默认 5000，上限 20000）、`backendId` / `start` / `end`。

**响应 TypeScript 定义**：`IPStats[]`（CH 路径补齐 geo）。

---

### 1.27 `GET /api/stats/hourly`

**说明**：按小时聚合的流量曲线，用于「近 N 小时流量」柱状/折线图。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `hours` | string（int） | 否 | `'24'` | 上限 **24 * 30 = 720** |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

注意控制器用的是 `service.parseLimit(hours, 24, 24 * 30)`。

**响应 TypeScript 定义**：`HourlyStats[]`

```ts
export interface HourlyStats {
  hour: string;        // 形如 "2024-01-01T13:00:00"（见 §6.3 toHourKey）
  upload: number;
  download: number;
  connections: number;
}
```

**说明**：CH 路径下若未传时间范围，`resolveQueryRange(timeRange, hours*60)` 会自动构造 `[now - hours*60 分钟, now]` 的查询区间（`Math.max(1, hours) * 60`）。

---

### 1.28 `GET /api/stats/trend`

**说明**：按分钟粒度的流量趋势（不聚合、尽量细）。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `minutes` | string（int） | 否 | `'30'` | 上限 **60 * 24 * 7 = 10080** |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

**响应 TypeScript 定义**（`packages/shared/src/index.ts`，原样；`stats.types.ts` 有完全相同的定义）：

```ts
export interface TrafficTrendPoint {
  time: string;
  upload: number;
  download: number;
}
```

**说明**（`timeseries.repository.ts` `getTrafficTrend`）：
- 若 `end - start > 6 小时` → 读 `hourly_stats`（`SELECT hour as time, upload, download`，`ORDER BY hour ASC`）。
- 否则 → 读 `minute_stats`（`SELECT minute as time, ...`，`ORDER BY minute ASC`）。
- `time` 字段即对应表的时间键字符串。

---

### 1.29 `GET /api/stats/trend/aggregated`

**说明**：把趋势数据按自定义时间桶（bucket）聚合，供图表使用，并支持按客户端时区做「天」对齐。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 取值规则（源码） |
|---|---|---|---|---|
| `minutes` | string（int） | 否 | `'30'` | `parseLimit(minutes, 30, 60*24*7)` → 上限 **10080** |
| `bucketMinutes` | string（int） | 否 | `'1'` | `parseLimit(bucketMinutes, 1, 24*60)` → 上限 **1440** |
| `tzOffsetMinutes` | string（int） | 否 | `0` | 仅当 `typeof === 'string'` 才 `Number()`；`Number.isFinite` 后 `Math.max(-840, Math.min(840, Math.trunc(v)))`；否则 0 |
| `backendId` / `start` / `end` | — | 否 | — | §0.3 |

**响应 TypeScript 定义**：`TrafficTrendPoint[]`

```ts
export interface TrafficTrendPoint {
  time: string;
  upload: number;
  download: number;
}
```

**说明**（`timeseries.repository.ts` `getTrafficTrendAggregated`）：
- `bucketMinutes >= 60` → 从 `hourly_stats` 聚合（60 倍更少行数）。
- 其中 `bucketMinutes >= 1440` 且 `tzOffsetMinutes !== 0` → 用 `strftime('%Y-%m-%dT%H:%M:00', hour, @offset, 'start of day', @reverseOffset)` 把 UTC 小时键先平移到调用方本地日、截断到「当天 00:00」、再平移回 UTC 输出。
- `bucketMinutes < 60` → 从 `minute_stats` 聚合。
- 另注：`stats.service.ts` 的 `getTrafficTrendAggregatedBase` 在 `bucketMinutes >= 1440 && tzOffsetMinutes !== 0` 时走 `db.repos.timeseries.getTrafficTrendAggregated(...)`（**绕过 db 层缓存**，因为「legacy DB facade cache has no timezone dimension」）。

---

### 1.30 `GET /api/stats/connections`

**说明**：历史「最近连接列表」。**当前实际恒返回空数组**。

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 范围 |
|---|---|---|---|---|
| `limit` | string（int） | 否 | `'100'` | 上限 **2000** |
| `backendId` | — | 否 | 激活 backend | §0.3(A)。**本接口不解析 `start`/`end`** |

**响应 TypeScript 定义**：源码标注 `any[]`；实际实现见下。

**关键事实**（`db.ts`）：
```ts
/**
 * @deprecated connection_logs is no longer written to (replaced by aggregation tables).
 * Kept for API backward compatibility — always returns [].
 */
getRecentConnections(backendId: number, _limit = 100): Connection[] {
  return [];
}
```
而 `getRecentConnectionsWithRouting` 在 CH 启用时调用 `clickHouseReader.getRecentConnections(backendId, limit)`，否则调用上面的 `db.getRecentConnections`（返回 `[]`）。

> **Go 重写建议**：保留该路由返回 `[]`（HTTP 200），或按需实现。若实现，元素类型应为 shared 的 `Connection`（见 §5.3）。

---

## 2. `/api/gateway/*` 与 `/api/agent/*`

### 2.1 `/api/gateway/*`（4 条）

所有 gateway 接口共用 `backendId` 解析：`getBackendIdFromQuery(query)` → `statsService.resolveBackendId(typeof query.backendId === 'string' ? query.backendId : undefined)`。解析失败 → **404 `{"error":"No backend specified or active"}`**；backend 不存在 → **404 `{"error":"Backend not found"}`**。

这三条路由的本质是**把上游 Clash/Surge 的 HTTP API 透传/整形**给前端，因此「响应结构」是上游结构，不是本项目定义的 TS 接口。

#### 2.1.1 `GET /api/gateway/proxies`

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `backendId` | string（int） | 否 | 激活 backend | |

**响应结构**（按 backend 类型分支）

稳定返回结构为：
```ts
{ proxies: Record<string, { name: string; type: string; now?: string }> }
```

- **Agent 模式**（`backend.url` 匹配 `/^agent:\/\//i`）：读 `realtimeStore.getAgentConfigWithPolicyState(backendId)`。
  - 未同步 → **503 `{"error":"Agent config not yet synced"}`**
  - 已同步 → `{ proxies: cached.proxies || {}, _source: 'agent-cache' }`（多一个 `_source` 字段）
- **Surge**：`GET {gateway}/v1/policies` → `{ proxies: string[]; 'policy-groups': string[] }`；对每个 policy group 再 `GET {gateway}/v1/policies/{groupName}`（timeout 5000ms）取 `detail.policy` 作为 `now`；group 记为 `{ name, type: 'Selector', now }`，叶子代理记为 `{ name, type: 'Unknown' }`。
  - 上游非 2xx → 透传状态码 + `{"error":"Surge API error: <status>"}`
- **Clash/OpenClash**：直接 `GET {gateway}/proxies` 并把 JSON **原样返回**（结构为 mihomo 的 `/proxies` 响应，不是本项目类型）。
  - 上游非 2xx → 透传状态码 + `{"error":"Gateway API error: <status>"}`
- 网络异常 → **502 `{"error":"<message>"}`**

#### 2.1.2 `GET /api/gateway/providers/proxies`

**Query 参数**

| 名称 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `backendId` | string（int） | 否 | 激活 backend | |
| `refresh` | string | 否 | — | **仅字符串 `'true'` 时** `forceRefresh = true`（Surge 专用） |

**响应结构**

- **Agent 模式**：未同步 → **503**；已同步 → `{ providers: cached.providers || {}, proxies: cached.proxies || {}, _source: 'agent-cache' }`
- **Surge**：
  ```ts
  {
    providers: Record<string, { proxies: { name: string; type: string; now?: string }[] }>,
    _cache?: { cached: boolean; lastUpdate: string; policyCount: number }   // 可选
  }
  ```
  - 命中缓存且非 forceRefresh：从 `db.getSurgePolicyCache(backendId)` 构造，每个 policy group 一条 proxy 记录 `{ name: policyGroup, type: policyType, now: selectedPolicy }`。
  - 否则直连 `GET {gateway}/v1/policies`（timeout 10000ms），再对每个 group `GET {gateway}/v1/policy_groups/select?group_name=<urlencoded>`（timeout 5000ms）取 `{ policy, type }`。
  - 另有 `providers['default'] = { proxies: data.proxies.map(name => ({name, type:'Unknown'})) }` 承载独立代理。
  - 上游失败且 providers 为空 → **502 `{"error":"Failed to fetch policies","message":"..."}`**
- **Clash**：直接透传上游 `GET {gateway}/providers/proxies` 的 JSON。

#### 2.1.3 `POST /api/gateway/providers/proxies/refresh`

**请求体**：无（无 body schema）。

**Query 参数**：`backendId`（可选）。

**校验顺序与错误**

| 条件 | 状态码 | Body |
|---|---|---|
| `backendId` 解析失败 | 404 | `{"error":"No backend specified or active"}` |
| backend 不存在 **或** `type !== 'surge'` | 400 | `{"error":"Only Surge backend supports this operation"}` |
| backend 是 Agent 模式 | 400 | `{"error":"Agent mode backend does not support this operation"}` |
| `policySyncService` 未注入 | 503 | `{"error":"Policy sync service not available"}` |

**响应结构**（源码内联）：
```ts
{
  success: boolean;
  message: string;
  updated: number;
}
```
（取值来自 `policySyncService.syncNow(backendId, gatewayBaseUrl, backend.token || undefined)` 的 `result.success` / `result.message` / `result.updated`。）

#### 2.1.4 `GET /api/gateway/rules`

**Query 参数**：`backendId`（可选）。

**响应结构**

- **Agent 模式**：
  - 未同步 → **503 `{"error":"Agent config not yet synced"}`**
  - `type === 'surge'`：`{ rules: Array<{type: string; payload: string; proxy: string; size: 0}>, _source: 'agent-cache' }`（由 `cached.rules[].raw` 经 `parseSurgeRule` 解析）
  - 其他：`{ rules: cached.rules || [], _source: 'agent-cache' }`
- **Surge**：
  ```ts
  {
    rules: Array<{ type: string; payload: string; proxy: string; size: 0 }>,
    _source: 'surge',
    _availablePolicies: string[]
  }
  ```
- **Clash**：透传上游 `GET {gateway}/rules` 的 JSON（`{ rules: Array<Record<string, unknown>>, ... }`），但会做一次 **RuleSet size 增强**：
  - 先 `GET {gateway}/providers/rules`（timeout 5000ms）建立 `providerName.toLowerCase() → ruleCount` 映射；
  - 对 `type === 'RuleSet'` 且 `size` 缺失、`payload` 非空的规则，用 `payload.toLowerCase()` 查表填入 `size`。
  - 若 `data.rules` 为空或映射为空 → 原样返回 `data`。
- 异常 → **502 `{"error":"<message>"}`**

**`parseSurgeRule` 的解析规则**（`packages/shared/src/gateway-utils.ts`，因为 Surge 规则是字符串，需还原成结构化对象）：

| 输入形态 | 输出 |
|---|---|
| 空行 / `#` 开头 | `null` |
| 按 `,` 分割后不足 2 段 | `null` |
| `FINAL,<policy>` | `{ type: 'MATCH', payload: '*', policy: unquote(parts[1]) \|\| 'DIRECT' }` |
| `GEOIP,<payload>,<policy>` | `{ type: 'GEOIP', payload: parts[1] \|\| '', policy: unquote(parts[2]) \|\| 'DIRECT' }` |
| `RULE-SET,<payload>,<policy>` | `{ type: 'RULE-SET', payload: parts[1] \|\| '', policy: unquote(parts[2]) \|\| 'DIRECT' }` |
| 一般 `TYPE,payload,policy`（≥3 段） | `{ type, payload: parts[1], policy: unquote(parts[2]) }` |
| 兜底 `TYPE,policy` | `{ type, payload: '', policy: unquote(parts[1]) \|\| 'DIRECT' }` |

`unquote(str) = str.replace(/^["']|["']$/g, '') || ''`。

---

### 2.2 `/api/agent/*`（4 条）

Agent 是「反向推送」模式：由部署在路由器上的 agent 主动把流量数据 POST 给 collector，而不是 collector 去连 WebSocket。

#### 2.2.1 通用鉴权（4 条路由共用）

`isAgentBackendAuthorized(backendId, request, reply)`，按顺序：

1. `db.getBackend(backendId)` 不存在 → **404 `{"error":"Backend not found"}`**（源码此处就是 `return false;` 但**没有 `return reply...`**，属于源码笔误 —— 实际是 `reply.status(404).send(...)` 后 `return false`）。
2. `!isAgentBackendUrl(backend.url)`（url 不以 `agent://` 开头）→ **400 `{"error":"Backend is not in agent mode (url must start with agent://)"}`**
3. `backend.token` 为空 → **403 `{"error":"Agent backend token is not configured"}`**
4. Token 提取 `parseAgentToken(request)`：
   - 优先 `Authorization: Bearer <token>`（`slice(7).trim()`）
   - 否则读自定义头 `x-agent-token`
5. `!provided || !timingSafeStringEqual(provided, expected)` → **401 `{"error":"Invalid agent token"}`**

`timingSafeStringEqual` 用 `crypto.timingSafeEqual`，长度不等直接 `false`。

#### 2.2.2 通用版本校验 —— `isAgentCompatible()`

| 环境变量 | 含义 | 默认 |
|---|---|---|
| `MIN_AGENT_PROTOCOL_VERSION` | 最小协议版本 | `1` |
| `MIN_AGENT_VERSION` | 最小 agent 版本（semver 前缀比较） | 空（不校验） |

- `protocolVersion` 缺失按 `1` 处理；小于阈值 →
  **426** `{"error":"...","code":"AGENT_PROTOCOL_TOO_OLD","minProtocolVersion":n,"receivedProtocolVersion":n}`
- `MIN_AGENT_VERSION` 非空时，取 `body.agentVersion || body.version`，用 `normalizeVersion`（去 `agent-v`/`v` 前缀）后按 `^(\d+)\.(\d+)(?:\.(\d+))?` 解析：
  - 解析失败 → **426** `{"error":"...","code":"AGENT_VERSION_REQUIRED","minAgentVersion":"..."}`
  - 低于最小值 → **426** `{"error":"...","code":"AGENT_VERSION_TOO_OLD","minAgentVersion":"...","receivedAgentVersion":"..."}`

#### 2.2.3 通用绑定校验 —— `isAgentBindingAllowed()`

- 无 heartbeat 记录 → 允许。
- `heartbeat.agentId === agentId` → 允许。
- 否则若上次 `lastSeen` 距今 **> 10000ms**（`AGENT_BINDING_TIMEOUT_MS`）→ 允许重新绑定。
- 否则 → **409**
  ```ts
  {
    error: "Agent token is already bound to '<agentId>'. Wait <n>s ...",
    code: "AGENT_TOKEN_ALREADY_BOUND",
    boundAgentId: string,
    remainingSeconds: number
  }
  ```

#### 2.2.4 `POST /api/agent/heartbeat`

**请求体 schema**（源码 `AgentHeartbeatPayload`，原样）：

```ts
type AgentHeartbeatPayload = {
  backendId?: number;
  agentId?: string;
  protocolVersion?: number;
  agentVersion?: string;
  hostname?: string;
  version?: string;
  gatewayType?: string;
  gatewayUrl?: string;
  gatewayLatencyMs?: number;
  serverLatencyMs?: number;
};
```

> 注：`backendId` 声明为可选 `number`，但实际必须能 `parseInt` 成 `> 0` 的整数，否则 **400 `{"error":"Invalid backendId"}`**。

**字段处理**
| 字段 | 处理 |
|---|---|
| `agentId` | `String(v||'').trim().slice(0,128)`，空 → **400 `{"error":"Invalid agentId"}`** |
| `hostname` | trim，截断 128，空则 `undefined` |
| `version` | 取 `agentVersion \|\| version`，trim，截断 64 |
| `gatewayType` | trim，截断 16 |
| `gatewayUrl` | trim，截断 512 |
| `gatewayLatencyMs` | 仅接受 `number`、finite、`>= 0`，否则 `undefined`；接受后 `Math.round` |
| `serverLatencyMs` | 同上 |
| `remoteIP` | `request.ip`，否则 `x-forwarded-for` 第一段 |

**响应结构**（源码内联）：
```ts
{ success: true, backendId: number, agentId: string, serverTime: string }
```

**副作用**：`db.upsertAgentHeartbeat({ backendId, agentId, hostname, version, gatewayType, gatewayUrl, remoteIP, gatewayLatencyMs, serverLatencyMs, lastSeen })`。

#### 2.2.5 `POST /api/agent/report`

**请求体 schema**（源码 `AgentReportPayload` + `AgentTrafficUpdatePayload`，原样）：

```ts
type AgentTrafficUpdatePayload = {
  domain?: string;
  ip?: string;
  chain?: string;
  chains?: string[];
  rule?: string;
  rulePayload?: string;
  upload?: number;
  download?: number;
  connections?: number;
  sourceIP?: string;
  timestampMs?: number;
};

type AgentReportPayload = {
  backendId?: number;
  agentId?: string;
  protocolVersion?: number;
  agentVersion?: string;
  requestId?: string;
  updates?: AgentTrafficUpdatePayload[];
};
```

**单条 update 的清洗规则**（`sanitizeAgentTrafficUpdate`，原样逻辑）：

| 字段 | 规则 |
|---|---|
| `upload` / `download` | 非 finite → 0；否则 `max(0, floor(v))`。**两者同时为 0 → 整条丢弃（返回 `null`）** |
| `chains` | 非数组则 `[]`；每项 `String().trim()` 去空；**最多 12 个** |
| 归一化 chains | 若清洗后为空 → `[String(raw.chain \|\| 'DIRECT').trim() \|\| 'DIRECT']` |
| `timestampMs` | 非 finite → `Date.now()`；否则 `max(0, floor(v))` |
| `connections` | 非 finite → `undefined`；否则 `max(0, floor(v))` |
| `domain` | trim，**截断 253** |
| `ip` | trim，**截断 64** |
| `chain` | `normalizedChains[0] \|\| 'DIRECT'` |
| `rule` | trim，截断 256，空 → `'Match'` |
| `rulePayload` | trim，截断 512 |
| `sourceIP` | trim，截断 64 |

**处理流程**

1. `backendId` 校验（400）→ 鉴权 → 版本校验 → `agentId` 校验（400）→ 绑定校验（409）。
2. **幂等**：`requestId`（截断 64）若在 5 分钟内出现过 → 立即返回
   ```ts
   { success: true, backendId, accepted: 0, dropped: 0, duplicate: true }
   ```
   `seenRequestIds` Map 以 requestId 为 key，TTL 5 分钟，每 60 秒按时间戳清理一次。
3. `updates` 非数组或空 → `{ success: true, backendId, accepted: 0, dropped: 0 }`。
4. **批大小上限**：`AGENT_INGEST_MAX_BATCH_SIZE`（默认 `5000`），`rawUpdates.slice(0, max)`。
5. 逐条 `sanitize`，丢弃 null；若全部被丢弃 → `{ success: true, backendId, accepted: 0, dropped: picked.length }`。
6. 对每条 update：
   - 若 `connections` 为 `undefined`（老 agent），用复合键去重后确定是否为 `1`：
     `` `${domain}:${ip}:${chains.join(' > ')}:${rule}:${rulePayload}:${sourceIP||''}` ``
     已在 `agentCounted` 集合中 → `0`，否则 `1` 并加入集合。
   - IP 非空且非 `0.0.0.0`/`::` → 累加到 `geoBatchByIp`。
   - `agentBuffer.add(backendId, {...})` 进 `BatchBuffer`（30s 窗口聚合，按 `(domain, ip, chain, fullChain, rule, rulePayload, sourceIP, minute)` 去重）。
   - `realtimeStore.recordTraffic(...)` 立即进实时内存。
7. GeoIP 异步查询（不阻塞响应），结果 `realtimeStore.recordCountryTraffic` + `agentBuffer.addGeoResult`。
8. `db.upsertAgentHeartbeat({backendId, agentId, lastSeen, remoteIP})`。
9. `onTrafficIngested?.(backendId)` 触发 WebSocket 广播。

**响应结构**（源码内联）：
```ts
{ success: true, backendId: number, accepted: number, dropped: number }
// 或（幂等命中）
{ success: true, backendId: number, accepted: 0, dropped: 0, duplicate: true }
```
- `accepted` = 通过清洗的条数
- `dropped` = `picked.length - updates.length`

**默认 flush 周期**：`AGENT_FLUSH_INTERVAL_MS`（默认 `30000`，最小 `5000`）。

#### 2.2.6 `POST /api/agent/config`

**请求体 schema**（源码 `AgentConfigPayload`，原样）：

```ts
type AgentConfigPayload = {
  backendId?: number | string;
  agentId?: string;
  config: {
    rules: Array<{ type: string; payload: string; proxy: string; raw?: string }>;
    proxies: Record<string, { name: string; type: string; now?: string }>;
    providers: Record<string, { proxies: Array<{ name: string; type: string; now?: string }> }>;
    timestamp: number;
    hash: string;
  };
};
```

**校验**：`backendId` → 鉴权 → `agentId` → 绑定；`config` 缺失 → **400 `{"error":"Missing config payload"}`**。
（注意：本路由**不**调用 `isAgentCompatible`。）

**响应结构**（源码内联）：
```ts
{ success: true, backendId: number, hash: string }
```

**副作用**：`realtimeStore.setAgentConfig(backendId, body.config)`（落 `agent_snapshots`）。

#### 2.2.7 `POST /api/agent/policy-state`

**请求体 schema**（源码内联类型）：

```ts
{
  backendId: number;
  agentId: string;
  policyState: {
    proxies: Record<string, unknown>;
    providers: Record<string, unknown>;
    timestamp: number;
  };
}
```

**校验**：`backendId` → 鉴权；`policyState` 缺失 → **400 `{"error":"Missing policyState payload"}`**。（**不校验 `agentId`**，也**不做绑定校验**。）

**响应结构**（源码内联）：
```ts
{ success: true, backendId: number, timestamp: number }
```

---

## 3. 鉴权机制

### 3.1 `/api/auth/*` 路由（5 条）

前缀 `/api/auth`（`authController`）。

| 方法 | 路径 | 是否公开 | 说明 |
|---|---|---|---|
| GET | `/api/auth/state` | ✅ 公开 | 查询鉴权状态 |
| POST | `/api/auth/verify` | ✅ 公开 | 校验 token，成功则下发 cookie |
| POST | `/api/auth/logout` | ✅ 公开 | 清除 cookie |
| POST | `/api/auth/enable` | ❌ 需鉴权 | 开启鉴权并设置 token |
| POST | `/api/auth/disable` | ❌ 需鉴权 | 关闭鉴权 |
| PUT | `/api/auth/token` | ❌ 需鉴权 | 更换 token |

> 公开路由白名单在 `app.ts` 的 `onRequest` 钩子里，按 `request.url.startsWith(route)` 判断：
> `/health`、`/api/auth/state`、`/api/auth/verify`、`/api/auth/logout`、`/api/agent/heartbeat`、`/api/agent/report`、`/api/auth/config`、`/api/agent/policy-state`。
> （源码里还列了 `/api/auth/config`，但 auth 模块**并未注册该路由** —— 属冗余条目。）

#### 3.1.1 `GET /api/auth/state`

**请求体/参数**：无。

**响应 TypeScript 定义**（`auth.types.ts` 与 `packages/shared/src/index.ts` 同构，原样）：

```ts
export interface AuthState {
  enabled: boolean;
  hasToken: boolean;
  forceAccessControlOff?: boolean;
  showcaseMode?: boolean;
}
```

**取值逻辑**：
```ts
const forcedOff = this.isForceAccessControlOff();   // process.env.FORCE_ACCESS_CONTROL_OFF === 'true'
return {
  enabled: forcedOff ? false : config.enabled,
  hasToken: !!config.tokenHash,
  forceAccessControlOff: forcedOff,
  showcaseMode: this.isShowcaseMode(),              // process.env.SHOWCASE_SITE_MODE === 'true'
};
```

#### 3.1.2 `POST /api/auth/verify`

**请求体 schema**（源码内联）：
```ts
{ token?: string }
```

**错误**：无 `token` → **400** `{"valid":false,"message":"Token is required"}`

**响应 TypeScript 定义**（`auth.types.ts`，原样）：
```ts
export interface AuthVerifyResult {
  valid: boolean;
  message?: string;
}
```
成功：`{ valid: true }`；失败：`{ valid: false, message: 'Invalid token' }`（**HTTP 200**）。

**成功时的 Cookie 下发**（`auth.controller.ts` 原样参数）：
```ts
const isSecure = request.protocol === 'https';
reply.setCookie('neko-session', token, {
  path: '/',
  httpOnly: true,
  secure: isSecure,          // 只有真实 HTTPS 连接才加 Secure
  sameSite: 'strict',
  maxAge: 60 * 60 * 24 * 7,  // 7 天
});
```
> ⚠️ Cookie 里存的是**明文 token**，不是签名/加密后的 session id。

#### 3.1.3 `POST /api/auth/logout`

无 body。响应：`{ success: true }`。副作用：`reply.clearCookie('neko-session', { path: '/' })`。

#### 3.1.4 `POST /api/auth/enable`

**请求体**：`{ token?: string }`。

| 条件 | 状态码 | Body |
|---|---|---|
| showcase 模式 | 403 | `{"error":"Forbidden"}` |
| 无 token | 400 | `{"error":"Token is required"}` |
| token 格式非法 | 400 | `{"error":"Invalid token format. Token must be at least 6 characters and contain both letters and numbers."}` |
| 成功 | 200 | `{"success":true,"message":"Authentication enabled successfully"}` |

#### 3.1.5 `POST /api/auth/disable`

**请求体**：`{ token?: string }`。

| 条件 | 状态码 | Body |
|---|---|---|
| showcase 模式 | 403 | `{"error":"Forbidden"}` |
| 需要鉴权且未强制关闭，且 cookie 无效且未提供 token | 401 | `{"error":"Token is required to disable authentication"}` |
| token 无效 | 401 | `{"error":"<verifyResult.message \|\| 'Invalid token'>"}` |
| 成功 | 200 | `{"success":true,"message":"Authentication disabled successfully"}` |

#### 3.1.6 `PUT /api/auth/token`

**请求体**：`{ currentToken?: string; newToken?: string }`。

| 条件 | 状态码 | Body |
|---|---|---|
| showcase 模式 | 403 | `{"error":"Forbidden"}` |
| 需要鉴权且未强制关闭，且 cookie 无效且无 `currentToken` | 401 | `{"error":"Current token is required"}` |
| `currentToken` 无效 | 401 | `{"error":"<message \|\| 'Invalid current token'>"}` |
| 无 `newToken` | 400 | `{"error":"New token is required"}` |
| 新 token 格式非法 | 400 | `{"error":"Invalid token format. ..."}` |
| 成功 | 200 | `{"success":true,"message":"Token updated successfully"}` |

### 3.2 Cookie 名称与签名方式

| 项 | 值 |
|---|---|
| Cookie 名 | **`neko-session`** |
| 内容 | **明文 token**（不是 HMAC 签名的 session） |
| Cookie 属性 | `path=/`、`httpOnly=true`、`secure=(protocol==='https')`、`sameSite='strict'`、`maxAge=604800`（7 天） |
| Cookie 签名 | 注册了 `@fastify/cookie` 的 `secret`（`COOKIE_SECRET` 或 DB 持久化的 `getOrCreateCookieSecret()`），但 `setCookie` 时**没有传 `signed: true`** → **实际未使用签名 cookie** |

> **结论**：签名密钥只用于插件初始化（可能为 `request.cookies` 的 signed-cookie 解析预留），**`neko-session` 本身是未签名 cookie**。Go 重写只需读写一个同名明文 cookie，无需实现签名。

### 3.3 Token 存储与校验

**存储**：`auth_config` 表（key-value），默认值：
```sql
INSERT OR IGNORE INTO auth_config (key, value) VALUES ('enabled', '0'), ('token_hash', '');
```

**哈希**（`auth.service.ts`）：
```ts
async function hashToken(token: string): Promise<string> {
  const crypto = await import('crypto');
  return crypto.createHash('sha256').update(token).digest('hex');
}
```
→ **SHA-256 十六进制小写**，无盐、无 KDF 迭代。

**校验 `verifyToken(token)` 完整逻辑**：
1. `isForceAccessControlOff()`（`FORCE_ACCESS_CONTROL_OFF === 'true'`）→ `{ valid: true }`（直接放行）。
2. `!config.enabled || !config.tokenHash` → `{ valid: true }`（未开启鉴权即放行）。
3. `hashToken(token)` 与 `config.tokenHash` 做 **`crypto.timingSafeEqual`**（长度不等先 `false`）→ 相等则有效。
4. **兼容重试**：若不等，再 `decodeURIComponent(token)`，若结果与原 token 不同，则对解码值再算一次 SHA-256 并做 `timingSafeEqual`。
   - 原因（源码注释）：WebSocket 鉴权直接读原始 `Cookie` 头，值可能仍是 percent-encoded。
   - `decodeURIComponent` 抛错（畸形百分号编码）→ 直接视为无效。
5. 否则 `{ valid: false, message: 'Invalid token' }`。

**Token 格式约束 `isValidTokenFormat`**：
```ts
if (!token || token.length < 6) return false;
const hasLetter = /[a-zA-Z]/.test(token);
const hasNumber = /[0-9]/.test(token);
return hasLetter && hasNumber;
```
→ **至少 6 字符，且同时包含字母与数字**。

**是否需要鉴权 `isAuthRequired()`**：
```ts
if (this.isForceAccessControlOff()) return false;
const config = this.db.getAuthConfig();
return config.enabled && !!config.tokenHash;
```

### 3.4 全局鉴权中间件（HTTP）

`app.ts` 注册的 `onRequest` 钩子，逻辑顺序：

1. `request.url.startsWith(<publicRoutes 中任意一项>)` → **直接放行**。
2. `!authService.isAuthRequired()` → 放行。
3. 读 `request.cookies['neko-session']`，`verifyToken` 有效 → 放行。
4. 否则看 `Authorization: Bearer <token>`：
   - 缺失或不以 `Bearer ` 开头 → **401 `{"error":"Authentication required"}`**
   - token 无效 → **401 `{"error":"<verifyResult.message \|\| 'Invalid token'>"}`**

> 即：**Cookie 优先，Bearer 兜底**。

### 3.5 双开关：`FORCE_ACCESS_CONTROL_OFF` 与 `SHOWCASE_SITE_MODE`

| 环境变量 | 效果 |
|---|---|
| `FORCE_ACCESS_CONTROL_OFF=true` | `verifyToken` 恒 `valid:true`；`isAuthRequired()` 恒 false；`/api/auth/state` 的 `enabled` 恒 false |
| `SHOWCASE_SITE_MODE=true` | 影响：① `/api/stats/*` 时间范围被强制 clamp 到最近 24h（见 §0.3(B)）；② `/api/auth/enable`、`/api/auth/disable`、`/api/auth/token` 返回 403；③ 所有 `/api/backends` 的写操作（POST/PUT/DELETE、`/:id/listening`、`/:id/test`、`/:id/rotate-agent-token`、`/test`）返回 403；④ `/api/db/cleanup`、`/api/db/vacuum`、`PUT /api/db/retention`、`PUT /api/db/geoip` 返回 403。**`POST /api/backends/:id/activate` 的 showcase 检查被显式移除**（允许演示模式切换 backend）。 |

---

## 4. 数据库表结构

### 4.1 说明

- SQLite 是**权威 schema**，全部 DDL 集中在 `apps/collector/src/database/schema.ts`。
- 每张业务表都以 `backend_id` 作为分区键，并有 `FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE`。
- 所有 `total_*` 流量字段类型为 `INTEGER DEFAULT 0`；时间字段为 `DATETIME`。
- 表名常量与 DDL 一一对应，`getAllSchemaStatements()` 按固定顺序建表后统一建索引。

### 4.2 核心表（题目指定的 9 张 + 实际承担聚合职责的表）

#### `connection_logs` — 连接日志（14 字段）

```sql
CREATE TABLE IF NOT EXISTS connection_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  backend_id INTEGER NOT NULL,
  domain TEXT,
  ip TEXT,
  chain TEXT,
  upload INTEGER DEFAULT 0,
  download INTEGER DEFAULT 0,
  timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：8**（`id`, `backend_id`, `domain`, `ip`, `chain`, `upload`, `download`, `timestamp`）

**索引（4 个）**
```sql
CREATE INDEX IF NOT EXISTS idx_connection_logs_backend   ON connection_logs(backend_id);
CREATE INDEX IF NOT EXISTS idx_connection_logs_timestamp ON connection_logs(timestamp);
CREATE INDEX IF NOT EXISTS idx_connection_logs_domain    ON connection_logs(domain);
CREATE INDEX IF NOT EXISTS idx_connection_logs_chain     ON connection_logs(chain);
```
> ⚠️ **该表已停止写入**。`db.ts` 明确注释：`connection_logs is no longer written to (replaced by aggregation tables)`。保留仅为 API 兼容。默认保留期 `retention.connection_logs_days = 7`。

#### `device_stats` — 设备（源 IP）维度（6 字段）

```sql
CREATE TABLE IF NOT EXISTS device_stats (
  backend_id INTEGER NOT NULL,
  source_ip TEXT NOT NULL,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  PRIMARY KEY (backend_id, source_ip),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：6**；**PK：(backend_id, source_ip)**
**索引**：无独立索引（PK 已覆盖 `backend_id` 前缀）。

#### `domain_stats` — 域名维度（9 字段）

```sql
CREATE TABLE IF NOT EXISTS domain_stats (
  backend_id INTEGER NOT NULL,
  domain TEXT NOT NULL,
  ips TEXT,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  rules TEXT,
  chains TEXT,
  PRIMARY KEY (backend_id, domain),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：9**；**PK：(backend_id, domain)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_domain_stats_backend ON domain_stats(backend_id);`

**`ips` / `rules` / `chains` 是逗号分隔的去重列表**，写入时用 `INSTR` 判重后追加，且有 **4000 字符硬上限**：
```sql
ips = CASE
  WHEN domain_stats.ips IS NULL THEN @ip
  WHEN LENGTH(domain_stats.ips) > 4000 THEN domain_stats.ips
  WHEN INSTR(',' || domain_stats.ips || ',', ',' || @ip || ',') > 0 THEN domain_stats.ips
  ELSE domain_stats.ips || ',' || @ip END
```

#### `ip_stats` — IP 维度（11 字段）

```sql
CREATE TABLE IF NOT EXISTS ip_stats (
  backend_id INTEGER NOT NULL,
  ip TEXT NOT NULL,
  domains TEXT,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  asn TEXT,
  geoip TEXT,
  chains TEXT,
  rules TEXT,
  PRIMARY KEY (backend_id, ip),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：11**；**PK：(backend_id, ip)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_ip_stats_backend ON ip_stats(backend_id);`
`geoip` 存 JSON（读时 `JSON.parse(row.geoIP)`）。shared 的 `IPStats` 没有 `rules` 字段，但表里有 —— API 输出时会丢弃。

#### `proxy_stats` — 代理链维度（6 字段）

```sql
CREATE TABLE IF NOT EXISTS proxy_stats (
  backend_id INTEGER NOT NULL,
  chain TEXT NOT NULL,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  PRIMARY KEY (backend_id, chain),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：6**；**PK：(backend_id, chain)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_proxy_stats_backend ON proxy_stats(backend_id);`
`chain` 存的是**完整链路**（`chains.join(' > ')`，如 `Proxy-A > GROUP > DIRECT`）。

#### `rule_stats` — 规则维度（7 字段）

```sql
CREATE TABLE IF NOT EXISTS rule_stats (
  backend_id INTEGER NOT NULL,
  rule TEXT NOT NULL,
  final_proxy TEXT,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  PRIMARY KEY (backend_id, rule),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：7**；**PK：(backend_id, rule)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_rule_stats_backend ON rule_stats(backend_id);`

#### `minute_stats` — 分钟级总量（5 字段）

```sql
CREATE TABLE IF NOT EXISTS minute_stats (
  backend_id INTEGER NOT NULL,
  minute TEXT NOT NULL,
  upload INTEGER DEFAULT 0,
  download INTEGER DEFAULT 0,
  connections INTEGER DEFAULT 0,
  PRIMARY KEY (backend_id, minute),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：5**；**PK：(backend_id, minute)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_minute_stats_backend_minute ON minute_stats(backend_id, minute);`
`minute` 格式：`YYYY-MM-DDTHH:MM:00`（见 §6.3）。

#### `hourly_stats` — 小时级总量（5 字段）

```sql
CREATE TABLE IF NOT EXISTS hourly_stats (
  backend_id INTEGER NOT NULL,
  hour TEXT NOT NULL,
  upload INTEGER DEFAULT 0,
  download INTEGER DEFAULT 0,
  connections INTEGER DEFAULT 0,
  PRIMARY KEY (backend_id, hour),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：5**；**PK：(backend_id, hour)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_hourly_stats_backend ON hourly_stats(backend_id);`
默认保留期 `retention.hourly_stats_days = 30`。

#### `country_stats` — 国家维度（8 字段）

```sql
CREATE TABLE IF NOT EXISTS country_stats (
  backend_id INTEGER NOT NULL,
  country TEXT NOT NULL,
  country_name TEXT,
  continent TEXT,
  total_upload INTEGER DEFAULT 0,
  total_download INTEGER DEFAULT 0,
  total_connections INTEGER DEFAULT 0,
  last_seen DATETIME,
  PRIMARY KEY (backend_id, country),
  FOREIGN KEY (backend_id) REFERENCES backend_configs(id) ON DELETE CASCADE
);
```
**字段数：8**；**PK：(backend_id, country)**
**索引**：`CREATE INDEX IF NOT EXISTS idx_country_stats_backend ON country_stats(backend_id);`

### 4.3 其余表（完整 schema，供 Go 重写时一并对齐）

| 表名 | 字段数 | PK | 关键索引 | 备注 |
|---|---|---|---|---|
| `rule_proxy_map` | 3（`backend_id`,`rule`,`proxy`） | `(backend_id, rule, proxy)` | `idx_rule_proxy_map(backend_id, rule, proxy)` | rule→proxy 多对多 |
| `asn_cache` | 4（`ip`,`asn`,`org`,`queried_at`） | `ip` | — | ASN 缓存 |
| `geoip_cache` | 10（`ip`,`country`,`country_name`,`city`,`asn`,`as_name`,`as_domain`,`continent`,`continent_name`,`queried_at`） | `ip` | — | GeoIP 缓存，`populateGeoIPs` 的源 |
| `device_domain_stats` | 7（`backend_id`,`source_ip`,`domain` + 4 指标） | `(backend_id, source_ip, domain)` | `idx_device_domain_source_ip(backend_id, source_ip)` | 设备×域名 |
| `device_ip_stats` | 7（`backend_id`,`source_ip`,`ip` + 4 指标） | `(backend_id, source_ip, ip)` | `idx_device_ip_source_ip(backend_id, source_ip)` | 设备×IP |
| `minute_dim_stats` | 10（`backend_id`,`minute`,`domain`,`ip`,`source_ip`,`chain`,`rule` + 3 指标） | 全部 7 维 | 见下方 6 个 idx | **区间查询主力表** |
| `minute_country_stats` | 8（`backend_id`,`minute`,`country`,`country_name`,`continent` + 3 指标） | `(backend_id, minute, country)` | `idx_minute_country_backend_minute` | 分钟级国家事实 |
| `hourly_dim_stats` | 10（同 `minute_dim_stats`，时间键为 `hour`） | 全部 7 维 | 见下方 6 个 idx | **>6 小时区间主力表** |
| `hourly_country_stats` | 8 | `(backend_id, hour, country)` | `idx_hourly_country_backend_hour` | 小时级国家事实 |
| `domain_proxy_stats` | 6（`backend_id`,`domain`,`chain` + 3 指标 + `last_seen`） | `(backend_id, domain, chain)` | `idx_domain_proxy_chain(backend_id, chain)` | 域名×代理 |
| `ip_proxy_stats` | 7（多一个 `domains TEXT`） | `(backend_id, ip, chain)` | `idx_ip_proxy_chain(backend_id, chain)` | IP×代理 |
| `rule_chain_traffic` | 6（`backend_id`,`rule`,`chain` + 3 指标 + `last_seen`） | `(backend_id, rule, chain)` | `idx_rule_chain_traffic(backend_id, rule)` | 规则×链路，链路图基线 |
| `rule_domain_traffic` | 6 | `(backend_id, rule, domain)` | `idx_rule_domain_traffic(backend_id, rule)` | 规则×域名 |
| `rule_ip_traffic` | 6 | `(backend_id, rule, ip)` | `idx_rule_ip_traffic(backend_id, rule)` | 规则×IP |
| `backend_configs` | 10（`id`,`name`,`url`,`token`,`type`,`enabled`,`is_active`,`listening`,`created_at`,`updated_at`） | `id` AUTOINCREMENT | `idx_backend_configs_name` **UNIQUE**(name) | 核心配置表 |
| `agent_heartbeats` | 11（`backend_id`,`agent_id`,`hostname`,`version`,`gateway_type`,`gateway_url`,`remote_ip`,`gateway_latency_ms`,`server_latency_ms`,`last_seen`,`updated_at`） | `backend_id` | `idx_agent_heartbeats_last_seen(last_seen)` | Agent 在线状态 |
| `agent_snapshots` | 4（`backend_id`,`config_json`,`policy_state_json`,`updated_at`） | `backend_id` | — | Agent 配置持久化 |
| `backend_health_logs` | 7（`backend_id`,`minute`,`status`,`latency_ms`,`server_latency_ms`,`message`） | `(backend_id, minute)` | `idx_backend_health_logs_backend_minute`、`idx_backend_health_logs_minute(minute)` | 每分钟一行 |
| `app_config` | 3（`key`,`value`,`updated_at`） | `key` | — | KV 配置 |
| `surge_policy_cache` | 6（`backend_id`,`policy_group`,`selected_policy`,`policy_type`,`all_policies`,`updated_at`） | `(backend_id, policy_group)` | `idx_surge_policy_backend`、`idx_surge_policy_updated` | Surge 策略缓存 |
| `auth_config` | 3（`key`,`value`,`updated_at`） | `key` | — | 鉴权 KV |

**`minute_dim_stats` / `hourly_dim_stats` 的 6 个索引**（对每个维度做覆盖，是区间查询性能关键）：
```sql
idx_minute_dim_backend_minute          (backend_id, minute)
idx_minute_dim_backend_minute_domain   (backend_id, minute, domain)
idx_minute_dim_backend_minute_ip       (backend_id, minute, ip)
idx_minute_dim_backend_minute_chain    (backend_id, minute, chain)
idx_minute_dim_backend_minute_rule     (backend_id, minute, rule)
idx_minute_dim_backend_minute_source   (backend_id, minute, source_ip)
-- hourly 同理，把 minute 换成 hour
```

**默认 `app_config` 值**：
```sql
INSERT OR IGNORE INTO app_config (key, value) VALUES
  ('retention.connection_logs_days', '7'),
  ('retention.hourly_stats_days', '30'),
  ('retention.auto_cleanup', '1'),
  ('geoip.lookup_provider', 'online'),
  ('geoip.online_api_url', 'https://api.ipinfo.es/ipinfo');
```

### 4.4 ClickHouse 表（可选数据源）

`apps/collector/src/modules/clickhouse/clickhouse.config.ts`。启用后用于替代 SQLite 做历史区间查询。

| 表 | 引擎 | ORDER BY | 字段 | TTL |
|---|---|---|---|---|
| `traffic_minute` | `SummingMergeTree` | `(backend_id, minute, domain, ip, source_ip, chain, rule)` | `backend_id UInt32, minute DateTime, domain String, ip String, source_ip String, chain String, rule String, upload UInt64, download UInt64, connections UInt32` | `minute + INTERVAL 90 DAY` |
| `country_minute` | `SummingMergeTree` | `(backend_id, minute, country)` | `backend_id UInt32, minute DateTime, country String, country_name String, continent String, upload UInt64, download UInt64, connections UInt32` | 90 天 |
| `traffic_agg` | `SummingMergeTree` | `(backend_id, minute)` | `backend_id UInt32, minute DateTime, upload UInt64, download UInt64, connections UInt32` | 90 天 |
| `traffic_detail` | `MergeTree` | `(backend_id, minute, domain, ip)` | 同 `traffic_minute` | 90 天 |
| `traffic_agg_buffer` | `Buffer` | — | `AS traffic_agg` | — |
| `traffic_detail_buffer` | `Buffer` | — | `AS traffic_detail` | — |
| `country_buffer` | `Buffer` | — | `AS country_minute` | — |

Buffer 参数（三张 buffer 表一致）：`Buffer(db, dest, 4, 10, 60, 100, 10000, 10000, 1000000)`（4 层、10–60s、100–10000 行、10KB–1MB）。
所有表 `PARTITION BY toYYYYMM(...)`、`SETTINGS index_granularity = 8192`。

> 读查询统一走 `*_buffer` 表（如 `FROM traffic_agg_buffer`），以读到尚未落盘的近期数据。

---

## 5. WebSocket 协议

### 5.1 服务端 WebSocket（对前端）

**启动**（`websocket.server.ts`）：
```ts
this.wss = new WSServer({
  port: this.port,
  host: '0.0.0.0',
  perMessageDeflate: false,
});
```
- 是**独立的 WS 端口**，不是 HTTP 升级（无 `path`、无 `noServer`）。
- 关闭 `perMessageDeflate`。

**鉴权（握手阶段）**

```ts
const url = new URL(req.url || '', `http://${req.headers.host}`);
let token = url.searchParams.get('token');
if (!token) token = readCookie(req.headers.cookie, 'neko-session');
if (this.authService.isAuthRequired()) {
  if (!token) { ws.close(4001, 'Authentication required'); return; }
  const verifyResult = await this.authService.verifyToken(token);
  if (!verifyResult.valid) { ws.close(4003, 'Invalid token'); return; }
}
```
- Token 来源优先级：**URL query `?token=` > Cookie `neko-session`**。
- 关闭码：`4001` 缺 token、`4003` token 无效、`4000` 校验过程内部异常。

**连接建立后立即推送一帧初始快照**（`sendStatsToClient(ws)`），随后由 `broadcastStats()` 按节流推送。

### 5.2 帧格式

**统一为 JSON 文本帧**（`JSON.stringify` / `JSON.parse`）。无二进制帧、无自定义分帧。

#### 客户端 → 服务端

消息类型：`'subscribe'` 或 `'ping'`（也声明了 `'stats'`/`'pong'` 但服务端未处理）。

**`WebSocketMessage` 完整定义**（`websocket.types.ts`，原样）：

```ts
export interface WebSocketMessage {
  type: 'stats' | 'ping' | 'pong' | 'subscribe';
  backendId?: number;
  start?: string;
  end?: string;
  minPushIntervalMs?: number;
  includeSummary?: boolean;
  summaryFields?: SummaryFieldKey[];

  // Trend
  includeTrend?: boolean;
  trendMinutes?: number;
  trendBucketMinutes?: number;

  // Device Detail (Source IP)
  includeDeviceDetails?: boolean;
  deviceSourceIP?: string;
  deviceDetailLimit?: number;

  // Proxy Detail
  includeProxyDetails?: boolean;
  proxyChain?: string;
  proxyDetailLimit?: number;

  // Rule Detail
  includeRuleDetails?: boolean;
  ruleName?: string;
  ruleDetailLimit?: number;
  includeRuleChainFlow?: boolean;

  // Pagination: Domains
  includeDomainsPage?: boolean;
  domainsPageOffset?: number;
  domainsPageLimit?: number;
  domainsPageSortBy?: string;
  domainsPageSortOrder?: string;
  domainsPageSearch?: string;

  // Pagination: IPs
  includeIPsPage?: boolean;
  ipsPageOffset?: number;
  ipsPageLimit?: number;
  ipsPageSortBy?: number;
  ipsPageSortOrder?: string;
  ipsPageSearch?: string;

  // Response Data (server to client)
  data?: StatsSummary;
  timestamp?: string;
}
```

> ⚠️ 上面 `ipsPageSortBy?: number` 在 `websocket.types.ts` 中确实写的是 `number`（**疑似笔误**）；`websocket.server.ts` 里**另有一份**同名定义，其中 `ipsPageSortBy?: string`，且 `timestamp` 为必填。Go 侧按 **`string`** 实现更安全。

**`ping` 处理**：立即回
```json
{ "type": "pong", "timestamp": "<ISO>" }
```

**`subscribe` 处理**：逐字段比对，只要**有任何字段变化**才重新推送；全部无变化则静默（计入 `subscribeNoop` 指标）。字段解析规则：

| 字段 | 解析规则 |
|---|---|
| `backendId` | `db.getBackend(id)` 存在才接受；不存在则 `console.warn` 并忽略 |
| `start`/`end` | 二者都缺 → `{}`（表示全量）；只传其一、解析失败、或 `start > end` → `parseRange` 返回 `null` → **该次不更新** |
| `minPushIntervalMs` | `Math.max(0, Math.min(60_000, Math.floor(v)))`；非 finite 保留旧值 |
| `includeSummary` | `v !== false` |
| `summaryFields` | 非数组 → 保留旧；空数组 → 重置为**全部 true**；否则只置 true 已知 key，若一个已知 key 都没有 → 保留旧 |
| `includeTrend` / `trendMinutes` / `trendBucketMinutes` | `includeTrend === false` → `null`（关闭）；三者全为 `undefined` → `undefined`（不变）；否则 `minutes = min(parsed ?? (range 推导或 30), 30*24*60)`，`bucketMinutes = min(parsed ?? 1, 24*60)` |
| `includeDeviceDetails` / `deviceSourceIP` / `deviceDetailLimit` | `false` → `null`；`sourceIP` 为空 → `undefined`（不变）；`limit = min(parsed ?? 5000, 20000)` |
| `includeProxyDetails` / `proxyChain` / `proxyDetailLimit` | 同上，`limit = min(parsed ?? 5000, 20000)` |
| `includeRuleDetails` / `ruleName` / `ruleDetailLimit` | 同上，`limit = min(parsed ?? 5000, 20000)` |
| `includeRuleChainFlow` | `!!v` |
| `includeDomainsPage` 系列 | `offset` 非负整数（默认 0）、`limit` 正整数（默认 50，**上限 200**）、`sortBy` 白名单（同 §1.4）、`sortOrder` 非 `'asc'` 一律 `'desc'`、`search` trim 后空则 `undefined`；`sortBy`/`sortOrder` 任一非法 → **整个 page 设置被忽略**（返回 `undefined` = 不变） |
| `includeIPsPage` 系列 | 同上，`sortBy` 白名单里 `domain` 换成 `ip` |

`summaryFields` 的合法 key（`SummaryFieldKey`）：
```ts
export type SummaryFieldKey =
  | 'totals'
  | 'topDomains'
  | 'topIPs'
  | 'proxyStats'
  | 'countryStats'
  | 'deviceStats'
  | 'ruleStats'
  | 'hourlyStats';
```
默认全部 `true`。

⚠️ **一个易踩的坑**：`domainsPageSortBy` 等字段只要**传了**且解析失败，`parseDomainsPage` 返回 `undefined`，`subscribe` 就**不会应用新配置**（也**不会**清空旧配置）—— 表现为「静默无效」。

#### 服务端 → 客户端（推送帧）

```ts
{
  type: 'stats',
  backendId: number | undefined,
  start?: string,
  end?: string,
  summaryFields?: SummaryFieldKey[],   // 仅 includeSummary 为真时存在
  data: StatsSummary,
  timestamp: string                    // ISO
}
```

**`data` 的类型**（`packages/shared/src/index.ts`，原样）：

```ts
export interface StatsSummary {
  totalUpload: number;
  totalDownload: number;
  totalConnections: number;
  totalDomains: number;
  totalIPs: number;
  totalProxies: number;
  totalRules?: number;
  todayUpload?: number;
  todayDownload?: number;
  activeConnections?: number;
  topDomains: DomainStats[];
  topIPs: IPStats[];
  proxyStats: ProxyStats[];
  countryStats?: CountryStats[];
  deviceStats?: DeviceStats[];
  deviceDetailSourceIP?: string;
  deviceDomains?: DomainStats[];
  deviceIPs?: IPStats[];
  proxyDetailChain?: string;
  proxyDomains?: DomainStats[];
  proxyIPs?: IPStats[];
  ruleDetailName?: string;
  ruleDomains?: DomainStats[];
  ruleIPs?: IPStats[];
  ruleChainFlowAll?: RuleChainFlowAll;
  domainsPage?: { data: DomainStats[]; total: number };
  domainsPageQuery?: {
    offset: number;
    limit: number;
    sortBy?: string;
    sortOrder?: "asc" | "desc";
    search?: string;
  };
  ipsPage?: { data: IPStats[]; total: number };
  ipsPageQuery?: {
    offset: number;
    limit: number;
    sortBy?: string;
    sortOrder?: "asc" | "desc";
    search?: string;
  };
  trendStats?: TrafficTrendPoint[];
  ruleStats?: RuleStats[];
  hourlyStats: HourlyStats[];
}
```

**字段填充规则**（`getStatsForBackend` 的 return 语句，原样映射）：

| 输出字段 | 来源 |
|---|---|
| `totalUpload` / `totalDownload` / `totalConnections` | `summary.totalUpload/...`，其中 `summary` 在 `includeSummaryData && summaryFields.totals` 时取缓存，否则为 `ZERO_BASE_SUMMARY`（全 0） |
| `totalDomains` | `summary.uniqueDomains` |
| `totalIPs` | `summary.uniqueIPs` |
| `totalProxies` | `summary.totalProxies` |
| `totalRules` | `summary.totalRules` |
| `topDomains` / `topIPs` / `proxyStats` / `hourlyStats` | 受 `summaryFields` 控制，未启用时分别为 `[]` |
| `countryStats` / `deviceStats` / `ruleStats` | 受 `summaryFields` 控制，未启用时为 `undefined` |
| `deviceDetailSourceIP` / `deviceDomains` / `deviceIPs` | 仅当订阅了 `includeDeviceDetails` 且 `deviceSourceIP` 非空 |
| `proxyDetailChain` / `proxyDomains` / `proxyIPs` | 仅当订阅了 `includeProxyDetails` |
| `ruleDetailName` / `ruleDomains` / `ruleIPs` | 仅当订阅了 `includeRuleDetails` |
| `ruleChainFlowAll` | 仅当 `includeRuleChainFlow` |
| `domainsPage` / `domainsPageQuery` | 仅当订阅了 `includeDomainsPage` |
| `ipsPage` / `ipsPageQuery` | 仅当订阅了 `includeIPsPage` |
| `trendStats` | 仅当订阅了 trend |

**注意 `ZERO_BASE_SUMMARY`**：
```ts
const ZERO_BASE_SUMMARY = {
  totalConnections: 0, totalUpload: 0, totalDownload: 0,
  uniqueDomains: 0, uniqueIPs: 0,
  totalProxies: 0, totalRules: 0,
};
```

**WS 默认时间范围**：未传 `start`/`end` 时构造
```ts
const defaultStart = new Date('2000-01-01T00:00:00.000Z').toISOString();
const defaultEnd   = new Date(now.getTime() + 86400000).toISOString();  // now + 24h
```
（注释：让 ClickHouse 能做全量聚合而不回退 SQLite。）

### 5.3 广播与节流

- `broadcastThrottleMs = 1000`（全局，非 force 时 1 秒内只广播一次）。
- 每个客户端还有独立的 `minPushIntervalMs` 节流。
- 相同查询组合（cacheKey）的 payload **只算一次**，用 `jsonCache: Map<string, Promise<string|null>>` 复用序列化结果。
- `cacheKey` 拼装：
  ```
  `${backendId}|${range.start||''}|${range.end||''}|${includeSummary?'1':'0'}|${summaryFieldsKey}|${trendKey}|${deviceDetailKey}|${proxyDetailKey}|${ruleDetailKey}|${ruleChainFlowKey}|${domainsPageKey}|${ipsPageKey}`
  ```
- **背压保护**：`ws.bufferedAmount > maxBufferedBytes`（`WS_MAX_BUFFERED_BYTES`，默认 4MB，最小 64KB）→ 跳过本轮发送（`lastSentAt` 不更新）。
- **基础摘要缓存**：`baseSummaryCache`，TTL 为 `BASE_SUMMARY_CACHE_TTL_MS = 2000`（当 `end` 接近 now）或 `BASE_SUMMARY_CACHE_TTL_HISTORICAL_MS = 300000`（5 分钟，历史区间）；上限 1000 条，超限丢最旧。

### 5.4 上游 Mihomo `/connections` 连接方式

`apps/collector/src/modules/collector/gateway.collector.ts`。

**连接参数**

```ts
const headers: Record<string, string> = {
  Origin: this.url.replace("ws://", "http://").replace("wss://", "https://"),
};
if (this.token) {
  headers["Authorization"] = `Bearer ${this.token}`;
}
this.ws = new WebSocket(this.url, { headers, followRedirects: true, ...gatewayWsTlsOptions() });
```

| 项 | 值 |
|---|---|
| URL | `backend.url` 原样（用户配置，形如 `ws://host:9090/connections`） |
| 认证头 | **`Authorization: Bearer <backend.token>`**（注意：这是 WS 连接，与 HTTP 的 `buildGatewayHeaders` 用的是同一套 Bearer） |
| Origin | 由 url 把 `ws://`→`http://`、`wss://`→`https://` 得来 |
| 重定向 | `followRedirects: true` |
| TLS | `gatewayWsTlsOptions()`：`BACKEND_TLS_INSECURE=1` 时跳过证书校验 |
| 心跳 | `ping()` 每 `WS_HEARTBEAT_INTERVAL_MS`（默认 **10000ms**）；超过 `WS_HEARTBEAT_TIMEOUT_MS`（默认 **30000ms**）无任何帧（含 pong）→ `ws.terminate()` 强制断开触发重连 |
| 重连 | `calculateBackoffDelay(attempt, reconnectInterval=5000, max=WS_MAX_RECONNECT_INTERVAL_MS \|\| 60000)`，**指数退避 + 抖动** |
| 陈旧连接清理 | `STALE_CONNECTION_TIMEOUT = 5 分钟`，`CLEANUP_INTERVAL = 2 分钟` |

**消息数据格式**（shared 的 `ConnectionsData`，即 mihomo `/connections` 的推送格式，原样）：

```ts
export interface ConnectionsData {
  downloadTotal: number;
  uploadTotal: number;
  connections: Connection[];
  memory?: number;
}

export interface Connection {
  id: string;
  metadata: ConnectionMetadata;
  upload: number;
  download: number;
  start: string;
  chains: string[];
  providerChains: string[];
  rule: string;
  rulePayload: string;
}

export interface ConnectionMetadata {
  network: string;
  type: string;
  sourceIP: string;
  destinationIP: string;
  sourceGeoIP: string[] | null;
  destinationGeoIP: string[] | null;
  sourceIPASN: string;
  destinationIPASN: string;
  sourcePort: string;
  destinationPort: string;
  inboundIP: string;
  inboundPort: string;
  inboundName: string;
  inboundUser: string;
  host: string;
  dnsMode: string;
  uid: number;
  process: string;
  processPath: string;
  specialProxy: string;
  specialRules: string;
  remoteDestination: string;
  dscp: number;
  sniffHost: string;
}
```

**关键字段的实际使用**（`createCollector` 的 `onData`）：
| 用途 | 取值 |
|---|---|
| domain | `metadata.host \|\| metadata.sniffHost \|\| ""` |
| ip | `metadata.destinationIP \|\| ""` |
| sourceIP | `metadata.sourceIP \|\| ""` |
| chains | `Array.isArray(conn.chains) ? conn.chains : ["DIRECT"]` |
| rule | `conn.rule \|\| "Match"` |
| rulePayload | `conn.rulePayload \|\| ""` |
| 流量 | `conn.upload` / `conn.download`（**累计值**，需做差） |
| 连接起点 | `conn.start`（用于 preexisting 判定） |

**`normalizeConnections` 兼容处理**：
- `id` 是 `number` → 转 `String`
- `upload`/`download` 支持 Stash 的 `{current, max, total, last}` 对象形态，通过 `normalizeCounter` 取 `.total`；非有限或负数 → 0

---

## 6. 采集与聚合逻辑

### 6.1 两条采集路径

| 路径 | 触发方式 | 入口 |
|---|---|---|
| **主动（gateway）** | collector 主动连 mihomo 的 `/connections` WS | `createCollector()` / `GatewayCollector` |
| **主动（surge）** | 轮询 Surge HTTP API | `surge.collector.ts` |
| **被动（agent）** | agent 反向上报 `POST /api/agent/report` | `app.ts` |

本节以 gateway 路径为重点（题目要求），agent 路径的清洗规则已在 §2.2.5 完整给出。

### 6.2 增量判定（去重）

**核心数据结构**：`activeConnections: Map<string, TrackedConnection>`

```ts
interface TrackedConnection {
  id: string;
  domain: string;
  ip: string;
  chains: string[];
  rule: string;
  rulePayload: string;
  lastUpload: number;
  lastDownload: number;
  totalUpload: number;
  totalDownload: number;
  counted: boolean;
  sourceIP?: string;
  lastSeen: number;
}
```

**mihomo 的 `upload`/`download` 是连接的累计值，因此必须做差。**

**新连接（`!existing`）**

```ts
const preexisting = isPreexistingConnection(conn.start, startedAt);
const hasInitialTraffic = !preexisting && (conn.upload > 0 || conn.download > 0);
...
counted: hasInitialTraffic || (preexisting && (conn.upload > 0 || conn.download > 0)),
```

- **preexisting 判定**（`connection-baseline.ts`）：
  ```ts
  export const PREEXISTING_GRACE_MS = 5_000;
  export function isPreexistingConnection(startValue: unknown, collectorStartedAtMs: number): boolean {
    const startMs = parseConnectionStartMs(startValue);
    if (startMs === null) return false;
    return startMs < collectorStartedAtMs - PREEXISTING_GRACE_MS;
  }
  ```
  `parseConnectionStartMs` 接受：ISO 字符串（`Date.parse`）、epoch 秒（`< 1e12` 时 × 1000）、epoch 毫秒。无法解析返回 `null` → **视为非 preexisting**（即首次快照会被计入）。
  > 这是对 issue #50 的修复：collector 重启时，之前就在传输的连接其累计值包含了 collector 未观测期间的数据，若直接计入会产生流量尖峰。故这类连接**只建基线，不计流量**。
- 只有 `hasInitialTraffic` 时才 `batchBuffer.add(..., connections: 1)` + `realtimeStore.recordTraffic(...)`。

**已存在连接（`existing`）**

```ts
if (conn.upload < existing.lastUpload || conn.download < existing.lastDownload) {
  // 计数器回退 = backend 重启 / conn id 复用
  uploadDelta = conn.upload;
  downloadDelta = conn.download;
  existing.counted = false;
  counterResets++;
} else {
  uploadDelta = conn.upload - existing.lastUpload;
  downloadDelta = conn.download - existing.lastDownload;
}
existing.lastUpload = conn.upload;    // 即使 delta 为 0 也刷新，避免被当 stale 清理后重复计入
existing.lastDownload = conn.download;
existing.lastSeen = now;
```

- **`connections` 语义**：一条连接**只计一次**连接数。
  ```ts
  const connections = existing.counted ? 0 : 1;
  if (connections > 0) existing.counted = true;
  ```
  计数器回退时会把 `counted` 重置为 `false`，下次有流量时再计 1。
- `existing.totalUpload/totalDownload` 自行累加 delta（仅用于内部追踪）。

**连接关闭**：本轮 `currentIds` 里没有的 id → 从 `activeConnections` 删除（剩余流量在关闭前的快照中已计入）。

### 6.3 分桶（minute / hour key）

`base.repository.ts`：

```ts
protected toMinuteKey(date: Date): string {
  return `${date.toISOString().slice(0, 16)}:00`;
}
protected toHourKey(date: Date): string {
  return `${date.toISOString().slice(0, 13)}:00:00`;
}
```

- **minute key**：`YYYY-MM-DDTHH:MM:00`（**UTC**，秒恒为 `00`）
- **hour key**：`YYYY-MM-DDTHH:00:00`（**UTC**，分秒恒为 `00`）

`batch-buffer.ts` 中的 `toMinuteKey(timestampMs)` 实现等价：
```ts
export function toMinuteKey(timestampMs?: number): string {
  const date = new Date(timestampMs ?? Date.now()).toISOString();
  return `${date.slice(0, 16)}:00`;
}
```

> **Go 重写注意**：分桶键是 **UTC 时间字符串**，不是本地时间，也不是 Unix 时间戳。写入的 `timestampMs` 来自消息到达时刻 `now`（gateway 路径）或 agent 上报的 `timestampMs`。

### 6.4 BatchBuffer：三级去重与 30 秒刷盘

**层 1：`BatchBuffer` 内部聚合（窗口内）**

```ts
add(backendId: number, update: TrafficUpdate) {
  const minuteKey = toMinuteKey(update.timestampMs);
  const fullChain = update.chains.join(" > ");
  const key = [
    backendId, minuteKey, update.domain, update.ip, update.chain,
    fullChain, update.rule, update.rulePayload, update.sourceIP || "",
  ].join(":");
  ...
}
```
同 key 的多次 add 会**累加** `upload`/`download`/`connections`，并保留较大的 `timestampMs`。

**层 2：`batchUpdateTrafficStats` 内部按维度再聚合**

见 §6.5，把一批 update 按 11 种维度各自聚合成 Map，减少 UPSERT 冲突。

**层 3：SQLite `UPSERT ... ON CONFLICT DO UPDATE`**

主键冲突时做 `total_xxx = total_xxx + @xxx` 累加。

**刷盘周期与环境变量**

| 变量 | 默认 | 说明 |
|---|---|---|
| `FLUSH_INTERVAL_MS` | `30000` | gateway 路径刷盘周期 |
| `FLUSH_MAX_BUFFER_SIZE` | `5000` | buffer 达到该条数**立即刷盘** |
| `AGENT_FLUSH_INTERVAL_MS` | `30000`（最小 5000） | agent 路径刷盘周期 |
| `AGENT_INGEST_MAX_BATCH_SIZE` | `5000` | agent 单次 report 最大条数 |

**刷盘并发保护**：`isFlushing` 标志，避免慢速 ClickHouse 写入期间定时器叠加第二次刷盘。`disconnect()` 时会 `while (isFlushing) await sleep(25)` 再最后 flush 一次。

**写入失败的三态处理**（`TrafficWriteOutcome`）：
| detailOk | aggOk | 动作 |
|---|---|---|
| ✅ | ✅ | `realtimeStore.clearTraffic(id)`（清全部实时增量） |
| ✅ | ❌ | `realtimeStore.clearTrafficDimensions(id)`（只清维度侧） |
| ❌ | ✅ | `realtimeStore.clearTrafficSummary(id)`（只清汇总侧） |
| ❌ | ❌ | 保留实时数据，且若 `sqliteSkipped` 则**回写 SQLite 兜底** |

### 6.5 写库：一次 flush 展开到哪些表

`traffic-writer.repository.ts` `batchUpdateTrafficStats(backendId, updates, reduceWrites)`。

**前置推导（每条 update）**

```ts
const ruleName   = buildRuleName(update);                                  // 见 §6.6
const finalProxy = update.chains.length > 0 ? update.chains[0] : 'DIRECT'; // 第一跳
const fullChain  = update.chains.join(' > ') || update.chain || 'DIRECT';
const { hourKey, minuteKey } = getTimeKeys(update.timestampMs ?? now.getTime());
```
跳过条件：`update.upload === 0 && update.download === 0`。

**聚合维度 Map（11 个）**

| Map | 聚合键 | 目标表 | 备注 |
|---|---|---|---|
| `domainMap` | `domain:ip:fullChain` | `domain_stats` | `ips`/`rules`/`chains` 逗号追加（4000 字符上限） |
| `ipMap` | `ip:domain:fullChain` | `ip_stats` | |
| `chainMap` | `fullChain` | `proxy_stats` | |
| `ruleProxyMap` | `ruleName:finalProxy` | `rule_stats` + `rule_proxy_map` | |
| `hourlyMap` | `hourKey` | `hourly_stats` | |
| `ruleChainMap` | `ruleName:fullChain` | `rule_chain_traffic` | |
| `ruleDomainMap` | `ruleName:domain` | `rule_domain_traffic` | 仅 `update.domain` 非空 |
| `ruleIPMap` | `ruleName:ip` | `rule_ip_traffic` | |
| `minuteMap` | `minuteKey` | `minute_stats` | |
| `minuteDimMap` | `minuteKey:domain:ip:sourceIP:fullChain:ruleName` | `minute_dim_stats` | **区间查询主力** |
| `hourlyDimMap` | `hourKey:domain:ip:sourceIP:fullChain:ruleName` | `hourly_dim_stats` | |
| `domainProxyMap` | `domain:fullChain` | `domain_proxy_stats` | 仅 `update.domain` 非空 |
| `ipProxyMap` | `ip:fullChain` | `ip_proxy_stats` | 额外维护 `domains` Set |
| `deviceMap` | `sourceIP` | `device_stats` | 仅 `update.sourceIP` 非空 |
| `deviceDomainMap` | `sourceIP:domain` | `device_domain_stats` | 需同时有 `sourceIP` 与 `domain` |
| `deviceIPMap` | `sourceIP:ip` | `device_ip_stats` | 需同时有 `sourceIP` 与 `ip` |

> 实为 16 个 Map（源码中全部声明）。所有写入在**一个事务 `tx1`** 内完成。

**`reduceWrites` 模式**（`shouldReduceSqliteWrites(clickHouseHealthy)`）：ClickHouse 健康时减少 SQLite 写入量（具体跳过哪些表由 `stats-write-mode.ts` 决定；`shouldSkipSqliteStatsWrites()` 为真时**完全跳过** SQLite stats 写入）。

### 6.6 规则名归一化 `buildRuleName`

`apps/collector/src/shared/utils/rule-name.ts`（原样）：

```ts
export type RuleNameInput = {
  rule: string;
  rulePayload: string;
  chains: string[];
};

export function buildRuleName(input: RuleNameInput): string {
  const rule = input.rule.trim();
  const rulePayload = input.rulePayload.trim();
  const lastChain = input.chains[input.chains.length - 1]?.trim() || '';

  if (input.chains.length > 1 && lastChain) {
    return lastChain;
  }

  if (rule) {
    return rulePayload ? `${rule}(${rulePayload})` : rule;
  }

  return lastChain || 'DIRECT';
}
```

**规则（Go 必须精确复刻）**
1. 链路**多于 1 跳**且最后一跳非空 → 返回**最后一跳的名字**（"顶层策略组名"）。
2. 单跳或空链路：`rule` 非空时 → `rule` 或 `rule(payload)`。
3. 兜底 → 最后一跳，再兜底 `'DIRECT'`。

> 源码注释说明了动机：前端拿 `rule` 去匹配 gateway 的 `rule.proxy`，而 mihomo 在 `rule` 字段上报的是规则**类型**（`RuleSet`/`IPCIDR`/`Match`…），不能作为多跳链路的聚合键。Surge 则把真实规则名放进 `rule` 并把同名镜像为最后一跳，因此该规则对 Surge 同样成立。

### 6.7 实时增量（RealtimeStore）与「是否叠加」

相关表写入 DB，但**在 flush 之前**的增量必须让前端立刻看到。`RealtimeStore` 保存这些内存增量，查询时按需叠加。

**叠加判定**

`stats.service.ts`：
```ts
shouldIncludeRealtime(timeRange: TimeRange): boolean {
  if (!timeRange.active) return true;
  if (!timeRange.end) return false;
  const endMs = new Date(timeRange.end).getTime();
  if (Number.isNaN(endMs)) return false;
  const toleranceMs = parseInt(process.env.REALTIME_RANGE_END_TOLERANCE_MS || '120000', 10);
  const windowMs = Number.isFinite(toleranceMs) ? Math.max(10_000, toleranceMs) : 120_000;
  return endMs >= Date.now() - windowMs;
}
```
- 未传时间范围（`active: false`）→ **始终叠加**。
- 传了范围且 `end` 在 `now - REALTIME_RANGE_END_TOLERANCE_MS`（默认 **120000ms = 2 分钟**，下限 10s）之内 → 叠加。
- 否则不叠加（纯历史查询）。

WS 端 `message.parser.ts` 有完全相同的逻辑。

> ⚠️ **注意两处 `shouldIncludeRealtime`**：`stats.service.ts` 与 `message.parser.ts` 各一份，实现一致但需在 Go 里只实现一次。

### 6.8 查询时的表选择（resolveFactTable）

`base.repository.ts`：

```ts
// 区间 → 事实表选择
if (rangeMs > 6 * 60 * 60 * 1000) {          // > 6 小时
  return { table: 'hourly_dim_stats', startKey: toHourKey(start), endKey: toHourKey(end), timeCol: 'hour' };
}
return { table: 'minute_dim_stats', startKey: toMinuteKey(start), endKey: toMinuteKey(end), timeCol: 'minute' };
```

- **区间 > 6 小时** → `hourly_dim_stats`（注释：起始边界向下取整到小时，引入最多 ~59 分钟额外数据，对 6h+ 区间影响 < 1%，12h+ < 0.3%）。
- **≤ 6 小时** → `minute_dim_stats`。
- 无时间范围（`parseMinuteRange` 返回 null）→ 读各自的**预聚合快照表**（`domain_stats`、`ip_stats`、`proxy_stats`、`rule_stats`、`device_stats`、`country_stats`、`rule_*_traffic` 等）。

> **这是本系统最重要的行为分叉**：同一接口「带 `start`/`end`」与「不带」走的是**完全不同的表和聚合路径**，数值可能有细微差异（尤其 hourly 边界取整）。

### 6.9 GeoIP 补全流程

`gateway.collector.ts` / `app.ts` 的 agent 路径：

1. 每次 batch 内按 **destination IP** 聚合 `{upload, download, connections}` 到 `geoBatchByIp`。
2. 对每个 IP 调 `geoService.getGeoLocation(ip)`（异步，不阻塞）。
3. 成功后：
   - `realtimeStore.recordCountryTraffic(backendId, geo, upload, download, connections, ts)`
   - `batchBuffer.addGeoResult({ ip, geo, upload, download, connections, timestampMs })`
4. flush 时 `geoQueue` 转成 `CountryUpdate[]`：
   ```ts
   {
     country: r.geo.country || 'Unknown',
     countryName: r.geo.country_name || r.geo.country || 'Unknown',
     continent: r.geo.continent || 'Unknown',
     upload, download,
     connections: Math.max(0, Math.floor(r.connections ?? 1)),
     timestampMs,
   }
   ```
   → `db.batchUpdateCountryStats(...)` + `clickHouseWriter.writeCountryBatch(...)`。

**读取时的补齐**：`populateGeoIPs()` 从 `geoip_cache` 表按 IP 批量查（`db.getIPGeolocations(ipsToLookup)`），构造
```ts
geoIP = { countryCode, countryName, city, asOrganization }
asn   = cached.asn
```
并遵循「DB 缓存优先，原有值兜底」：`geoIP: geoMap.get(ip) || stat.geoIP`。

### 6.10 Agent 路径与 gateway 路径的语义对齐

源码注释明确说明 agent 模式「mirrors direct mode's 30s batch window」：

- agent 的每条 update 也进 `BatchBuffer`（同一份去重逻辑）。
- `BatchBuffer` 的 key 含 `minute`，所以**一条长连接在 1 分钟内只计 1 次连接数**，与 gateway 路径的 `existing.counted` 语义一致。
- 老 agent 不带 `connections` 时，用 `agentCounted` Set + 复合键 `domain:ip:chains.join(' > '):rule:rulePayload:sourceIP` 做**窗口内去重**（flush 后随 `clearTraffic` 一起 `agentCountedByBackend.delete(backendId)`）。

---

## 7. 附录：其余模块接口（README 级摘要）

### 7.1 `/health`

`GET /health` → `{ "status": "ok" }`。**公开路由**。

### 7.2 `/api/backends/*`（13 条，`backend.controller.ts`）

| 方法 | 路径 | showcase 限制 | 请求体 | 响应 |
|---|---|---|---|---|
| GET | `/` | — | — | `BackendResponse[]` |
| GET | `/active` | — | — | `BackendResponse \| undefined` |
| GET | `/listening` | — | — | `BackendResponse[]` |
| POST | `/` | 403 | `CreateBackendInput` | `CreateBackendResult`；重名 → **409 `{"error":"Backend name already exists"}`**；缺 name/url → 400 |
| PUT | `/:id` | 403 | `UpdateBackendInput` | 更新结果；不存在 → 404 |
| DELETE | `/:id` | 403 | — | 删除结果；不存在 → 404 |
| POST | `/:id/activate` | **无限制**（检查被移除） | — | 激活结果；不存在 → 404 |
| POST | `/:id/listening` | 403 | `{ listening: boolean }` | 结果；不存在 → 404 |
| POST | `/:id/test` | 403 | — | `TestConnectionResult`；不存在 → 404 |
| POST | `/:id/rotate-agent-token` | 403 | — | `RotateAgentTokenResult`；不存在 → 404 |
| POST | `/test` | 403 | `TestConnectionInput` | `TestConnectionResult` |
| GET | `/health/history` | — | query `from`/`to`/`backendId` | `BackendHealthHistory[]`；`backendId` 非法 → 400 |

> 表中列出 13 行，与源码中 `fastify.get/post/put/delete` 的出现次数（13）一致。

**类型定义**（`backend.types.ts`，原样）：
```ts
export interface CreateBackendInput {
  name: string;
  url: string;
  token?: string;
  type?: 'clash' | 'surge';
}

export interface UpdateBackendInput {
  name?: string;
  url?: string;
  token?: string;
  type?: 'clash' | 'surge';
  enabled?: boolean;
  listening?: boolean;
}

export interface TestConnectionInput {
  url: string;
  token?: string;
  type?: 'clash' | 'surge';
}

export interface TestConnectionResult {
  success: boolean;
  message: string;
}

export interface CreateBackendResult {
  id: number;
  isActive: boolean;
  message: string;
  agentToken?: string;
}

export interface RotateAgentTokenResult {
  message: string;
  agentToken: string;
}

export interface BackendHealthPoint {
  time: string;
  status: 'healthy' | 'unhealthy' | 'unknown';
  latency_ms: number | null;
  server_latency_ms: number | null;
  message: string | null;
}

export interface BackendHealthHistory {
  backendId: number;
  backendName: string;
  points: BackendHealthPoint[];
}

export interface BackendHealthInfo {
  status: 'healthy' | 'unhealthy' | 'unknown';
  lastChecked: number;
  message?: string;
  latency?: number;
  serverLatency?: number;
}

export interface BackendResponse {
  id: number;
  name: string;
  url: string;
  token: string;
  type: 'clash' | 'surge';
  enabled: boolean;
  is_active: boolean;
  listening: boolean;
  created_at: string;
  updated_at: string;
  health?: BackendHealthInfo;
}
```

> ⚠️ 注意 `BackendResponse.token` 是**明文**返回给前端的。

### 7.3 `/api/db/*`（7 条，`config.controller.ts`）

| 方法 | 路径 | showcase 限制 |
|---|---|---|
| GET | `/stats` | — |
| POST | `/cleanup` | 403 |
| POST | `/vacuum` | 403 |
| GET | `/retention` | — |
| PUT | `/retention` | 403 |
| GET | `/geoip` | — |
| PUT | `/geoip` | 403 |

`/cleanup` 会顺带 `db.clearRangeQueryCache(backendId)`、`realtimeStore.clearBackend(...)`、`clearAgentRuntimeState(...)`、`notifyBackendDataCleared(...)`。

---

## 8. 重写风险点汇总

按风险从高到低排列。每条都给出了**具体源码依据**，便于逐项核对。

### 🔴 高风险（行为差异会被用户立刻发现）

1. **`country_stats` 有两个互相冲突的 TS 定义。**
   `stats.types.ts` 的 `CountryStats` **没有** `lastSeen`；`packages/shared/src/index.ts` 的有可选 `lastSeen`。`stats.service.ts` import 的是前者，`country.repository.ts` 用的是后者。`/api/stats/countries` 实际可能带 `lastSeen`。
   → Go 侧建议**始终输出可选 `lastSeen`**（`omitempty`）。

2. **`/api/stats/rules/domains/proxy-stats` 与 `/api/stats/rules/ips/proxy-stats` 在无时间范围时恒返回 `[]`。**
   依据：`rule.repository.ts` 的 `getRuleDomainProxyStats` / `getRuleIPProxyStats` 在 `parseMinuteRange(start,end)` 为 null 时**直接 `return []`**，无任何 fallback 查询。其它 5 个 `proxy-stats` 类接口（domains/ips 维度）都有降级到 `domain_proxy_stats` / `ip_proxy_stats` 的分支，这两个没有。这是**易被误判为 bug 的行为**，重写时必须显式保留或显式修掉（并同步告知前端）。

3. **`/api/stats/connections` 恒返回 `[]`。**
   `db.ts` 明确标注 `@deprecated`，`getRecentConnections` 直接 `return [];`。Go 侧若真的实现了查询，会与现有前端预期不一致（前端可能已不依赖）。

4. **`totalDomains` / `totalIPs` 不包含实时增量。**
   `getSummary` 里：`totalDomains: summary.uniqueDomains`、`totalIPs: summary.uniqueIPs` —— 而 `totalConnections/totalUpload/totalDownload` 用的是 `summaryWithRealtime`。同一个响应里**混用了叠加与未叠加的值**。重写时若「统一都叠加」会改变数值。

5. **`totalRules` / `totalProxies` 是数组长度而非 COUNT 查询。**
   `totalRules: ruleStats.length`、`totalProxies: proxyStats.length`。若 Go 侧改成 `SELECT COUNT(*)`，在「实时增量引入了 DB 中还没有的 rule/proxy」时数值会不同。

6. **6 小时阈值决定读哪张表，且 hourly 路径有最多 59 分钟的边界溢出。**
   `resolveFactTable`：`rangeMs > 6h → hourly_dim_stats`，且 `startKey` 向下取整到小时。源码注释自认「引入最多 ~59 min 额外数据」。Go 若精确按分钟边界聚合，长区间数值会与 TS 版**不一致**。

7. **`shouldIncludeRealtime` 的 120 秒容差与两处重复实现。**
   `REALTIME_RANGE_END_TOLERANCE_MS` 默认 `120000`，且 `Math.max(10_000, toleranceMs)` 保底 10s。`stats.service.ts` 与 `message.parser.ts` 各实现一份（逻辑相同）。漏掉这一层会让「最近一次 flush 之后」的数据消失（最多 30 秒空窗）。

8. **`sortOrder` 只认小写精确 `'asc'`。**
   ```ts
   const sortOrder = opts.sortOrder === 'asc' ? 'ASC' : 'DESC';
   ```
   `'ASC'`、`'ascending'` 全部按 DESC。WS 的 `normalizePageSortOrder` 略有不同（`sortOrder?.toLowerCase() === 'asc'`，**大小写不敏感**）。HTTP 与 WS **行为不一致**，重写需分别对齐。

9. **`sortBy` 非法时静默回退 `totalDownload`，不报错。**
   `sortColumnMap[opts.sortBy || 'totalDownload'] || 'totalDownload'` —— Go 若做了严格校验返回 400，会破坏前端。

10. **`parseInt` 的宽松性。**
    `resolveBackendId` 用 `Number.parseInt(raw, 10)`：`"3abc"` → `3`。Go 的 `strconv.Atoi` 会失败返回 `null` → 404。行为不同。

### 🟠 中风险（影响特定场景）

11. **Preexisting connection 基线（issue #50）的 5 秒宽限。**
    `PREEXISTING_GRACE_MS = 5000`，且依赖 gateway 与 collector **时钟大致同步**。源码注释自认有局限：「Bytes a preexisting connection moves between collector start and the first snapshot are not counted」。Go 重写若省略这一层，每次 collector 重启都会产生流量尖峰。

12. **计数器回退的处理。**
    `conn.upload < existing.lastUpload || conn.download < existing.lastDownload` 时把**当前值整个当新流量**并重置 `counted=false`。这是有意的（backend 重启 / id 复用），但如果 mihomo 偶发上报异常小值，会产生一笔虚假的大流量。Go 需原样复刻还是加阈值，需要产品决策。

13. **`buildRuleName` 的三分支逻辑。**
    多跳取最后一跳、单跳取 `rule(payload)`、兜底 `DIRECT`。这是**聚合键**，直接决定 `/api/stats/rules` 的全部内容以及 `rule_stats` / `rule_*_traffic` 表的键。任何偏差都会导致规则排行完全对不上。

14. **`connection_logs` 表已停写但仍保留 DDL 与 4 个索引。**
    Go 侧要么建表但不写，要么完全去掉并删掉 `/api/stats/connections`。中间状态（建表但没人写）会白占索引维护成本。

15. **`domain_stats.ips/rules/chains` 的 4000 字符硬截断与 `INSTR` 去重。**
    超过 4000 字符后**永久停止追加**（`ELSE domain_stats.ips`）。Go 若用 `string_agg` 会得到不同结果。且去重是**大小写敏感**的精确匹配（`INSTR(',' || ips || ',', ',' || @ip || ',')`）。

16. **`ip_stats.geoip` 存 JSON 字符串，读时 `JSON.parse`。**
    且 shared 的 `IPStats` **没有** `rules` 字段，但 `ip_stats` 表**有** `rules` 列 —— 读取时被丢弃。Go 侧注意别多输出字段（前端可能做严格类型校验）。

17. **`/api/stats/rule-proxy-map` 的返回形态与 shared 的同名类型完全不同。**
    接口返回 `Array<{rule: string; proxies: string[]}>`，而 `shared` 的 `RuleProxyMapping` 是 `{rule, proxy, totalUpload, totalDownload, totalConnections}`。不要把两者混淆。

18. **`/api/stats/rules/chain-flow` 用的是匿名内联类型（无命名接口、无 `layer`/`nodeType`/`rules`）。**
    与 `chain-flow-all`（`RuleChainFlowAll`，有 `layer`/`nodeType`/`rules`/`rulePaths`/`maxLayer`）结构**不同**。前端可能用同一个渲染器兼容两种形态，Go 侧必须分开实现。

19. **`chain-flow` 的 `links` 用数组下标而非 id。**
    且节点集合是由 `flowPath` 逐跳构建的（`flowPath.length < 2` 的直接丢弃），节点顺序依赖 `Map` 插入顺序 —— **不保证稳定**。若前端依赖节点顺序做布局，Go 侧需要用相同的插入顺序（`for (const row of rows)` 的遍历顺序，`rows` 来自 SQL 的 `GROUP BY chain` 无序）。这里是潜在的不确定性来源。

20. **`RULE_CHAIN_FLOW_MAX_ROWS` 默认 2000 的行数上限，且只在 SQLite 路径生效。**
    依据：`rule.repository.ts` 的 `getAllRuleChainFlows` / `getRuleChainFlow`（SQLite）使用 `getRuleChainFlowMaxRows()` 并在 SQL 里 `LIMIT ?`；而 `clickhouse.reader.ts` 的 `getAllRuleChainFlows` 经由 `private getRuleChainTrafficRows(...)` 取数，该查询**没有 `LIMIT`**。
    → 同一请求在「CH 启用 + 传了时间范围」与「CH 未启用」两种部署下会得到**不同规模的图**：SQLite 版只保留流量 Top-N 的 rule×chain，CH 版则是全量。行数超限时图会「缺边」，且缺的恰好是低流量部分。Go 侧需明确以哪一种为准。

21. **`/api/stats/global` 完全忽略 `start`/`end`。**
    控制器签名 `fastify.get('/global', async () => ...)` 没有 `request` 参数。前端如果给它传时间范围，参数会被静默丢弃。

22. **ClickHouse 只在「显式传 start 且 end」时启用。**
    `shouldUseForRange` 要求 `!!start && !!end`。因此「不传时间范围」= 一定读 SQLite 预聚合表（可能滞后，取决于 retention/rollup）。Go 若默认走 CH 全量聚合，数值会变。

23. **`CH_STRICT_STATS=1` 会把回退变成 500。**
    开启时 `failIfStrictFallback(route)` 直接 `throw new Error(...)`，Fastify 默认转 500 `{"error":"...","message":"...","statusCode":500}`。Go 侧需要决定是否保留这个「快速失败」开关。

### 🟡 低风险（实现细节）

24. **showcase 模式的时间 clamp 逻辑有 3 条隐式规则。**
    `start` 早于 24h 前 → clamp；`start > end` → 整体重置为 `[now-24h, now]`；任何解析失败 → 静默采用默认值而**不返回 400**。容易实现成「报错」而改变行为。

25. **`/api/gateway/providers/proxies` 的 `refresh` 只认字符串 `'true'`。**
    `query.refresh === 'true'` —— `'1'`、`'TRUE'` 都不生效。

26. **`parseSurgeRule` 对 `FINAL` 与 `MATCH` 的映射。**
    `FINAL` → `type: 'MATCH', payload: '*'`。这是个**类型改写**，前端拿 `MATCH` 去做匹配。

27. **Surge `policy-groups` 的两次不同上游调用。**
    `/api/gateway/proxies` 用 `GET /v1/policies/{groupName}`，而 `/api/gateway/providers/proxies` 用 `GET /v1/policy_groups/select?group_name=`。**两条路由调不同的 Surge API**，Go 侧要分别照抄，不能合并。

28. **Agent 幂等的 `requestId` TTL 是 5 分钟 + 60 秒惰性清理。**
    且 `isDuplicateRequestId` 的行为是「检查并记录」**一体**（首次调用即写入）。若 Go 侧实现成「先查后写」，在高并发下会有重复窗口。

29. **`isAgentBackendAuthorized` 中 404 分支的源码笔误。**
    ```ts
    if (!backend) {
      reply.status(404).send({ error: 'Backend not found' });
      return false;
    }
    ```
    这里**没有 `return reply...`**，只是 `return false`，调用方再 `return;`。语义等价，但说明这段代码没被严格审阅过 —— 建议 Go 侧写测试覆盖。

30. **WebSocket 是独立端口，且 `perMessageDeflate: false`。**
    Go 侧若用 `http.Server` 的 upgrade 复用 HTTP 端口，前端配置需要改；且必须显式禁用 permessage-deflate 以对齐（否则大 payload 的压缩比不同，不会影响语义但影响带宽）。

31. **WS 的 `WebSocketMessage` 有两份不完全相同的定义。**
    `websocket.types.ts` 的 `ipsPageSortBy?: number`（疑似笔误）+ `timestamp?: string`；`websocket.server.ts` 内联版本的 `ipsPageSortBy?: string` + `timestamp: string`（必填）。以 server 那份为准。

32. **`summaryFields: []`（空数组）的特殊语义。**
    `parseSummaryFields` 里空数组表示「**重置为全部 true**」，而不是「全部 false」。因为 `EMPTY_SUMMARY_FIELDS` 是全 false，容易被误实现成返回全 false。

33. **`minPushIntervalMs` 上限 60000，且 `0` 合法。**
    `Math.max(0, Math.min(60_000, Math.floor(v)))`。`0` 表示不节流（只受全局 1 秒节流约束）。

34. **Cookie 是明文 token，未签名。**
    虽然注册了 `@fastify/cookie` 的 `secret`，但 `setCookie` 未传 `signed: true`。Go 侧**不需要**实现 HMAC 签名，只需 `httpOnly` + `sameSite=strict` + 条件 `secure`。但要注意：**WS 鉴权会读原始 Cookie 头**，可能拿到 percent-encoded 值，所以 `verifyToken` 有「解码后重试」分支 —— Go 侧必须保留，否则某些浏览器/客户端下 WS 鉴权会失败。

35. **`decodeURIComponent` 的重试分支不能省。**
    见上。且要处理畸形百分号编码（TS 侧 `try/catch` 静默视为无效）。

36. **`sha256` 无盐、无 KDF 迭代。**
    `sha256(token).hex()` 直接与 DB 值比较。安全性上偏弱（token 要求「≥6 字符 + 含字母 + 含数字」也偏宽松），但为兼容已有数据库**必须原样沿用**，否则现有用户的 token 全部失效。

37. **`FORCE_ACCESS_CONTROL_OFF` 会同时影响 3 个地方。**
    `verifyToken` 恒真、`isAuthRequired()` 恒假、`/api/auth/state` 的 `enabled` 恒假。Go 侧要三处都改，只改一处会不一致。

38. **`/api/stats/devices/domains` 与 `/api/stats/devices/ips` 的 `sourceIP` 缺失不报 400。**
    其它「必填」参数（`domain`/`ip`/`rule`/`chain`）都返回 400，唯独 `sourceIP` 返回 `[]`。这是不一致但**已被前端依赖**的行为。

39. **时间参数解析用 `new Date(str)`，不是严格的 ISO 校验。**
    `new Date('2024')`、`new Date('2024-01')` 都能解析。Go 的 `time.Parse(time.RFC3339, ...)` 会拒绝这些。若做严格校验，会改变「哪些 start 值被接受」的行为。

40. **`clamp` 的上限 840 分钟。**
    `tzOffsetMinutes` 被 `Math.max(-840, Math.min(840, Math.trunc(v)))` 限制（±14 小时，覆盖现实世界最大时区偏移）。

---

### 重写建议（优先级排序）

1. **先固化类型层**：把 `packages/shared/src/index.ts` + `stats.types.ts` + `websocket.types.ts` + `auth.types.ts` + `backend.types.ts` 全部翻成 Go struct，并解决 `CountryStats` 的双定义冲突（取 shared 版）。
2. **再固化「解析层」**：`resolveBackendId` / `getTimeRange` / `parseLimit` / `parseOffset` / `shouldIncludeRealtime` / `resolveFactTable` / `buildRuleName` / `toMinuteKey` / `toHourKey`。这 9 个函数决定了 90% 的行为一致性，且都有明确的、可直接翻译的实现。
3. **然后写 29 个 stats 接口**，每个接口先实现「无时间范围（预聚合表）」路径，再实现「有时间范围（dim 表）」路径 —— 这两条路径在 TS 里就是两个独立方法，Go 里也应该是两个。
4. **为风险点 2/3/4/5/8/9/10 各写一条对照测试**（用同一个 SQLite 数据库跑 TS 与 Go，比对 JSON）。
5. **WS 单独实现**，先只支持 `type: 'stats'` 的初始快照 + 全字段订阅，再逐步加 `summaryFields` 裁剪、detail、paged、trend。
6. **Agent 路由最后做**，它只是「HTTP 入口 + 清洗 + 塞进同一个 BatchBuffer」，逻辑独立。
