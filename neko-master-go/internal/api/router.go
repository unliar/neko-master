// Package api 实现 HTTP 路由层。
//
// 路由与响应结构严格对齐上游 64 条契约，
// 但 Phase 1 只实现核心子集，其余按契约返回空结构占位。
package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/clickhouse"
	"github.com/unliar/neko-master/neko-master-go/internal/config"
	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
	"github.com/unliar/neko-master/neko-master-go/internal/geo"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
	"github.com/unliar/neko-master/neko-master-go/internal/realtime"
	"github.com/unliar/neko-master/neko-master-go/internal/stats"
	"github.com/unliar/neko-master/neko-master-go/internal/store"
)

// Server 持有 API 依赖。
type Server struct {
	cfg   *config.Config
	db    *store.DB
	stats *stats.Service
	geo   *geo.Lookup
	ch    *clickhouse.Writer // 为 nil 表示 CH 未启用，仅用于 /api/db/stats 统计
	log   *slog.Logger
	mux   *http.ServeMux
}

// NewServer 创建并注册路由。
func NewServer(cfg *config.Config, db *store.DB, log *slog.Logger) *Server {
	return NewServerWithRealtime(cfg, db, log, nil)
}

// NewServerWithRealtime 创建服务器并接入实时叠加管理器。
//
// rt 与 collector.Runner 共用同一份 Manager，才能让 API 看到
// 采集器刚写入、还没落盘的那部分增量。
func NewServerWithRealtime(cfg *config.Config, db *store.DB, log *slog.Logger, rt *realtime.Manager) *Server {
	var svc *stats.Service
	if rt != nil {
		svc = stats.NewWithRealtime(db, rt)
	} else {
		svc = stats.New(db)
	}
	s := &Server{
		cfg:   cfg,
		db:    db,
		stats: svc,
		log:   log,
		mux:   http.NewServeMux(),
	}
	s.geo = geo.New(s.geoProvider(), s.geoAPIURL(), cfg.GeoMMDBDir, db.SQL(), log)
	s.routes()
	return s
}

// Geo 暴露 GeoIP 查询器，供采集器按 IP 查国家（国家维度统计需要）。
func (s *Server) Geo() *geo.Lookup { return s.geo }

// AttachClickHouse 挂上 ClickHouse 写入器（nil 表示禁用）。
//
// 主要给 GET /api/db/stats 用：CH 启用时需要报告它的体积与行数，
// 以及图上所示的 size 等于 sqliteSize + clickhouseSize。
func (s *Server) AttachClickHouse(ch *clickhouse.Writer) { s.ch = ch }

// Handler 返回 http.Handler（已套访问控制中间件）。
func (s *Server) Handler() http.Handler { return s.authMiddleware(s.mux) }

func (s *Server) routes() {
	// 健康检查
	s.mux.HandleFunc("GET /health", s.handleHealth)

	// 统计
	s.mux.HandleFunc("GET /api/stats/summary", s.handleSummary)
	s.mux.HandleFunc("GET /api/stats/global", s.handleGlobal)
	s.mux.HandleFunc("GET /api/stats/domains", s.handleDomains)
	s.mux.HandleFunc("GET /api/stats/ips", s.handleIPs)
	s.mux.HandleFunc("GET /api/stats/proxies", s.handleProxies)
	s.mux.HandleFunc("GET /api/stats/rules", s.handleRules)
	s.mux.HandleFunc("GET /api/stats/devices", s.handleDevices)
	s.mux.HandleFunc("GET /api/stats/countries", s.handleCountries)
	s.mux.HandleFunc("GET /api/stats/hourly", s.handleHourly)
	s.mux.HandleFunc("GET /api/stats/trend", s.handleTrend)

	// 后端配置（列表带/不带斜杠都要注册）
	s.mux.HandleFunc("GET /api/backends", s.handleBackendsList)
	s.mux.HandleFunc("GET /api/backends/", s.handleBackendsList)
	s.mux.HandleFunc("GET /api/backends/active", s.handleBackendActive)
	s.mux.HandleFunc("GET /api/backends/listening", s.handleBackendListening)

	// 网关代理
	s.mux.HandleFunc("GET /api/gateway/proxies", s.handleGatewayProxies)
	s.mux.HandleFunc("GET /api/gateway/rules", s.handleGatewayRules)
	s.mux.HandleFunc("GET /api/gateway/providers/proxies", s.handleGatewayProviders)

	// 下钻接口（详情页必用）
	s.registerDrilldown()

	// 实时推送（WebSocket）
	s.mux.HandleFunc("GET /ws", s.handleWS)

	// 访问控制
	s.registerAuth()

	// Phase 2：backend CRUD / agent 上报 / 数据库运维
	s.registerBackends()
	s.registerAgent()
	s.registerDB()

	// 已废弃但保留兼容：/api/stats/connections 恒返回空
	s.mux.HandleFunc("GET /api/stats/connections", s.handleConnectionsLegacy)

	// 规则链路图
	s.mux.HandleFunc("GET /api/stats/rules/chain-flow", s.handleChainFlow)
	s.mux.HandleFunc("GET /api/stats/rules/chain-flow-all", s.handleChainFlowAll)
	s.mux.HandleFunc("GET /api/stats/rule-proxy-map", s.handleRuleProxyMap)

	// Surge 专用：刷新 provider
	s.mux.HandleFunc("POST /api/gateway/providers/proxies/refresh", s.handleProviderRefresh)

	// GeoIP 诊断（只读，上游无此路由）
	s.mux.HandleFunc("GET /api/db/geoip/status", s.handleGeoStatus)
	s.mux.HandleFunc("GET /api/db/geoip/lookup", s.handleGeoLookup)

	// 前端静态资源（若 web/out 存在）
	s.mountStatic()
}

// ---------------------------------------------------------------- 工具

func (s *Server) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	w.WriteHeader(code)
	if v == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.Error("encode json", "err", err)
	}
}

func (s *Server) writeErr(w http.ResponseWriter, code int, msg string) {
	s.writeJSON(w, code, map[string]string{"error": msg})
}

// parseRange 解析 start / end 查询参数。
//
// 前端所有统计接口都传时间范围（ISO8601）。早前的实现完全
// 没读这两个参数，等于时间过滤静默失效。
// 解析失败时返回 (nil, nil) —— 不报错，
// 因为上游也是静默忽略非法值。
func (s *Server) parseRange(q url.Values) (start, end *time.Time) {
	return parseTimeParam(q.Get("start")), parseTimeParam(q.Get("end"))
}

// parseTimeParam 解析单个时间参数，容庅多种格式。
func parseTimeParam(raw string) *time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02",
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, raw); err == nil {
			u := t.UTC()
			return &u
		}
	}
	return nil
}

// resolveBackendID 对齐上游 getBackendId：
// 传了但解析不出整数 → 0（调用方返回 404）；未传 → 当前激活 backend。
func (s *Server) resolveBackendID(r *http.Request) (int64, bool) {
	raw := r.URL.Query().Get("backendId")
	if raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, false
		}
		return id, true
	}
	id, err := s.stats.ActiveBackendID()
	if err != nil {
		return 0, false
	}
	return id, true
}

// parseLimit 对齐上游 parseLimit：缺省/非法/<=0 → fallback；超 max → 截断。
func parseLimit(raw string, fallback, max int) int {
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}

// parseOffset 对齐上游 parseOffset：缺省 0，负数按 0。
func parseOffset(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// getTimeRange 对齐上游 getTimeRange（非 showcase 模式）。
// 返回 (start, end, active, errMsg)。
func (s *Server) getTimeRange(r *http.Request) (time.Time, time.Time, bool, string) {
	q := r.URL.Query()
	rawStart, rawEnd := q.Get("start"), q.Get("end")

	if s.cfg.ShowcaseSiteMode {
		now := time.Now()
		return now.Add(-24 * time.Hour), now, true, ""
	}

	if rawStart == "" && rawEnd == "" {
		return time.Time{}, time.Time{}, false, ""
	}
	if rawStart == "" || rawEnd == "" {
		return time.Time{}, time.Time{}, false, "Both start and end must be provided together"
	}
	start, err1 := time.Parse(time.RFC3339, rawStart)
	end, err2 := time.Parse(time.RFC3339, rawEnd)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, false, "Invalid time range format, expected ISO datetime"
	}
	if start.After(end) {
		return time.Time{}, time.Time{}, false, "start must be less than or equal to end"
	}
	return start, end, true, ""
}

// geoProvider 读 GeoIP 数据源，回落到环境变量默认值。
func (s *Server) geoProvider() string {
	var v string
	if err := s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'geoip.lookup_provider'`).Scan(&v); err == nil && v != "" {
		return v
	}
	return s.cfg.GeoLookupProvider
}

// geoAPIURL 读在线查 IP 的接口地址。
func (s *Server) geoAPIURL() string {
	var v string
	if err := s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'geoip.online_api_url'`).Scan(&v); err == nil && v != "" {
		return v
	}
	return s.cfg.GeoOnlineAPIURL
}

// ---------------------------------------------------------------- 时间工具

var (
	nowFunc = time.Now
	hourDur = time.Hour
)

// ---------------------------------------------------------------- handlers

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, model.HealthResponse{Status: "ok"})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	if _, _, _, msg := s.getTimeRange(r); msg != "" {
		s.writeErr(w, http.StatusBadRequest, msg)
		return
	}
	res, err := s.stats.Summary(id)
	if err != nil {
		s.log.Error("summary", "backend", id, "err", err)
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleGlobal(w http.ResponseWriter, r *http.Request) {
	res, err := s.stats.Global()
	if err != nil {
		s.log.Error("global", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 50, 500)
	offset := parseOffset(q.Get("offset"))

	// 返回必须是 {data, total}（对齐上游 PaginatedDomainStats）。
	// 之前直接向前端向的形式固然错了。
	rStart, rEnd := s.parseRange(q)
	res, err := s.stats.DomainsPaged(id, stats.PageOptions{
		Offset:    offset,
		Limit:     limit,
		SortBy:    q.Get("sortBy"),
		SortOrder: q.Get("sortOrder"),
		Search:    q.Get("search"),
	}, rStart, rEnd)
	if err != nil {
		s.log.Error("domains", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleIPs(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 50, 500)
	offset := parseOffset(q.Get("offset"))

	// 必须返回 {data, total}（对齐上游 PaginatedIPStats）。
	// 之前向前端向的形式固然错了，这里同时修掉。
	rStart, rEnd := s.parseRange(q)
	res, err := s.stats.IPsPaged(id, stats.PageOptions{
		Offset:    offset,
		Limit:     limit,
		SortBy:    q.Get("sortBy"),
		SortOrder: q.Get("sortOrder"),
		Search:    q.Get("search"),
	}, rStart, rEnd)
	if err != nil {
		s.log.Error("ips", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"data":  s.attachGeoIPs(toAnySlice(res.Data)),
		"total": res.Total,
	})
}

func (s *Server) handleProxies(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	res, err := s.stats.AllProxies(id)
	if err != nil {
		s.log.Error("proxies", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	res, err := s.stats.AllRules(id)
	if err != nil {
		s.log.Error("rules", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 50, 500)
	offset := parseOffset(q.Get("offset"))

	res, err := s.stats.Devices(id, limit, offset)
	if err != nil {
		s.log.Error("devices", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleCountries(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 50, 500)
	offset := parseOffset(q.Get("offset"))

	res, err := s.stats.Countries(id, limit, offset)
	if err != nil {
		s.log.Error("countries", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleHourly(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	n := parseLimit(r.URL.Query().Get("limit"), 24, 720)
	res, err := s.stats.Hourly(id, n)
	if err != nil {
		s.log.Error("hourly", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTrend(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	start, end, active, msg := s.getTimeRange(r)
	if msg != "" {
		s.writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !active {
		// 未传时间范围：默认最近 24 小时
		end = time.Now()
		start = end.Add(-24 * time.Hour)
	}
	res, err := s.stats.Trend(id, start, end)
	if err != nil {
		s.log.Error("trend", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------- 网关代理

// gatewayClientFor 为指定 backend 构造网关客户端。
//
// 返回的 type 是标准化后的：clash / surge / agent。
func (s *Server) gatewayClientFor(id int64) (*gateway.Client, string, error) {
	be, err := s.stats.GetBackend(id)
	if err != nil {
		return nil, "", err
	}
	return gateway.New(be.URL, be.Token, s.cfg.BackendTLSInsecure), be.Type, nil
}

// handleGatewayProxies 对应 GET /api/gateway/proxies。
//
// Surge 分支（API-DETAILS §2.1.1）：
//
//	GET /v1/policies → {proxies, policy-groups}
//	对每个 group 再 GET /v1/policies/{groupName} 取 detail.policy 作为 now
//	group 记为 {name, type:Selector, now}，叶子代理记为 {name, type:Unknown}
//	上游非 2xx → 透传状态码 + {"error":"Surge API error: <status>"}
func (s *Server) handleGatewayProxies(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, typ, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	if strings.EqualFold(typ, "surge") {
		s.handleSurgeProxies(w, r, id)
		return
	}

	res, err := c.Proxies(r.Context())
	if err != nil {
		s.log.Warn("gateway proxies", "backend", id, "err", err)
		s.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// handleSurgeProxies 实现 Surge 形态的 proxies 响应。
func (s *Server) handleSurgeProxies(w http.ResponseWriter, r *http.Request, id int64) {
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	sc := gateway.NewSurgeClient(be.URL, be.Token, s.cfg.BackendTLSInsecure)
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	pol, err := sc.Policies(ctx)
	if err != nil {
		s.writeErr(w, http.StatusBadGateway, "Surge API error: "+err.Error())
		return
	}

	out := map[string]map[string]any{}
	for _, g := range pol.PolicyGroups {
		detail, err := sc.PolicyByName(ctx, g)
		if err != nil {
			s.log.Warn("surge policy detail", "group", g, "err", err)
			out[g] = map[string]any{"name": g, "type": "Selector"}
			continue
		}
		out[g] = map[string]any{"name": g, "type": "Selector", "now": detail.Policy}
	}
	for _, p := range pol.Proxies {
		out[p] = map[string]any{"name": p, "type": "Unknown"}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"proxies": out})
}

// handleGatewayRules 对应 GET /api/gateway/rules。
//
// Surge 分支（API-DETAILS §2.1.4）：规则是字符串，
// 需经 parseSurgeRule 还原成 {type,payload,policy,raw}，
// 并额外给出 _availablePolicies。
func (s *Server) handleGatewayRules(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, typ, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	if strings.EqualFold(typ, "surge") {
		be, err := s.stats.GetBackend(id)
		if err != nil {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		sc := gateway.NewSurgeClient(be.URL, be.Token, s.cfg.BackendTLSInsecure)
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		rd, err := sc.Rules(ctx)
		if err != nil {
			s.writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		// 字段名对齐上游 app.ts 的 Surge 分支：parseSurgeRule 给出 policy，
		// 但响应里改名为 proxy 并固定 size: 0。
		// 前端 interactive-rule-stats.tsx 硬读 rule.proxy，否则拿不到代理组名，
		// Surge 的零流量规则会被全部过滤掉。
		out := make([]map[string]any, 0, len(rd.Rules))
		for _, r := range rd.Rules {
			out = append(out, map[string]any{
				"type":    r.Type,
				"payload": r.Payload,
				"proxy":   r.Policy,
				"size":    0,
			})
		}
		s.writeJSON(w, http.StatusOK, map[string]any{
			"rules":              out,
			"_source":            "surge",
			"_availablePolicies": rd.AvailablePolicies,
		})
		return
	}

	res, err := c.Rules(r.Context())
	if err != nil {
		s.log.Warn("gateway rules", "backend", id, "err", err)
		s.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// handleGatewayProviders 对应 GET /api/gateway/providers/proxies。
//
// Surge 分支（API-DETAILS §2.1.2）：
//
//	GET /v1/policies → {proxies, policy-groups}
//	对每个 group GET /v1/policy_groups/select?group_name=<enc> 取 {policy,type}
//	注意这里用的端点与 /api/gateway/proxies 不同（风险点 27），
//	两条路由不能合并。
//	额外给出 providers["default"] 承载独立代理。
//	refresh 只识字符串 'true'（风险点 25）。
func (s *Server) handleGatewayProviders(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, typ, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	if strings.EqualFold(typ, "surge") {
		be, err := s.stats.GetBackend(id)
		if err != nil {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		sc := gateway.NewSurgeClient(be.URL, be.Token, s.cfg.BackendTLSInsecure)
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()

		pol, err := sc.Policies(ctx)
		if err != nil {
			s.writeErr(w, http.StatusBadGateway, "Failed to fetch policies: "+err.Error())
			return
		}

		// refresh=true 时刷新策略缓存
		if r.URL.Query().Get("refresh") == "true" {
			_, _ = s.db.SQL().Exec(`DELETE FROM surge_policy_cache WHERE backend_id = ?`, id)
		}

		providers := map[string]any{}
		for _, g := range pol.PolicyGroups {
			sel, err := sc.SelectPolicyGroup(ctx, g)
			if err != nil {
				s.log.Warn("surge select", "group", g, "err", err)
				continue
			}
			providers[g] = map[string]any{
				"name":    g,
				"type":    sel.Type,
				"now":     sel.Policy,
				"proxies": nil,
			}
			_, _ = s.db.SQL().Exec(`
				INSERT INTO surge_policy_cache (backend_id, policy_group, selected_policy, policy_type)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(backend_id, policy_group) DO UPDATE SET
					selected_policy = excluded.selected_policy,
					policy_type = excluded.policy_type,
					updated_at = CURRENT_TIMESTAMP`, id, g, sel.Policy, sel.Type)
		}

		// 独立代理承载在 providers["default"]
		defaultList := make([]map[string]any, 0, len(pol.Proxies))
		for _, p := range pol.Proxies {
			defaultList = append(defaultList, map[string]any{"name": p, "type": "Unknown"})
		}
		providers["default"] = map[string]any{"proxies": defaultList}

		s.writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
		return
	}

	// c.Providers 拿到的是 mihomo 原始响应里内层的 providers map（client.go 已解宊 json:"providers"）。
	// 上游 TS 版对 Clash 是整包 res.json() 透传，因此顶层仍然有 providers 外壳。
	// 前端 active-chain.ts 读 providers.providers，这里必须包回外壳，否则 providers 为 undefined 崩溃。
	res, err := c.Providers(r.Context())
	if err != nil {
		s.log.Warn("gateway providers", "backend", id, "err", err)
		s.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"providers": res})
}

// ---------------------------------------------------------------- backends

// maskToken 只表明 token 是否存在，不返回明文。
func maskToken(tok string) string {
	if tok == "" {
		return ""
	}
	return "****"
}

// ---------------------------------------------------------------- 静态资源

// mountStatic 在存在 web/out 时提供前端静态文件。
func (s *Server) mountStatic() {
	dir := "web/out"
	if strings.TrimSpace(dir) == "" {
		return
	}
	fs := http.FileServer(http.Dir(dir))
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// API 路径不应落到这里
		if strings.HasPrefix(r.URL.Path, "/api/") {
			s.writeErr(w, http.StatusNotFound, "not found")
			return
		}
		fs.ServeHTTP(w, r)
	})
}
