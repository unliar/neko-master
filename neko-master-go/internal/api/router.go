// Package api 实现 HTTP 路由层。
//
// 路由与响应结构严格对齐上游 64 条契约，
// 但 Phase 1 只实现核心子集，其余按契约返回空结构占位。
package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/config"
	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
	"github.com/unliar/neko-master/neko-master-go/internal/stats"
	"github.com/unliar/neko-master/neko-master-go/internal/store"
)

// Server 持有 API 依赖。
type Server struct {
	cfg   *config.Config
	db    *store.DB
	stats *stats.Service
	log   *slog.Logger
	mux   *http.ServeMux
}

// NewServer 创建并注册路由。
func NewServer(cfg *config.Config, db *store.DB, log *slog.Logger) *Server {
	s := &Server{
		cfg:   cfg,
		db:    db,
		stats: stats.New(db),
		log:   log,
		mux:   http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler 返回 http.Handler。
func (s *Server) Handler() http.Handler { return s.mux }

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

	// 网关代理
	s.mux.HandleFunc("GET /api/gateway/proxies", s.handleGatewayProxies)
	s.mux.HandleFunc("GET /api/gateway/rules", s.handleGatewayRules)
	s.mux.HandleFunc("GET /api/gateway/providers/proxies", s.handleGatewayProviders)

	// 后端配置
	s.mux.HandleFunc("GET /api/backends/", s.handleBackendsList)
	s.mux.HandleFunc("GET /api/backends/active", s.handleBackendActive)
	s.mux.HandleFunc("GET /api/backends/listening", s.handleBackendListening)

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

	res, err := s.stats.TopDomains(id, limit, offset)
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

	res, err := s.stats.TopIPs(id, limit, offset)
	if err != nil {
		s.log.Error("ips", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
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
func (s *Server) gatewayClientFor(id int64) (*gateway.Client, error) {
	be, err := s.stats.GetBackend(id)
	if err != nil {
		return nil, err
	}
	return gateway.New(be.URL, be.Token, s.cfg.BackendTLSInsecure), nil
}

func (s *Server) handleGatewayProxies(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
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

func (s *Server) handleGatewayRules(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
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

func (s *Server) handleGatewayProviders(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	c, err := s.gatewayClientFor(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	res, err := c.Providers(r.Context())
	if err != nil {
		s.log.Warn("gateway providers", "backend", id, "err", err)
		s.writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------- backends

func (s *Server) handleBackendsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SQL().Query(`
		SELECT id, name, url, type, enabled, is_active, listening
		FROM backend_configs ORDER BY id`)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []model.Backend{}
	for rows.Next() {
		var b model.Backend
		var enabled, isActive, listening int
		if err := rows.Scan(&b.ID, &b.Name, &b.URL, &b.Type, &enabled, &isActive, &listening); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		b.Enabled = enabled != 0
		b.IsActive = isActive != 0
		b.Listening = listening != 0
		out = append(out, b)
	}
	s.writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleBackendActive(w http.ResponseWriter, r *http.Request) {
	id, err := s.stats.ActiveBackendID()
	if err != nil {
		s.writeJSON(w, http.StatusOK, nil)
		return
	}
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeJSON(w, http.StatusOK, nil)
		return
	}
	s.writeJSON(w, http.StatusOK, be)
}

func (s *Server) handleBackendListening(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SQL().Query(`SELECT id FROM backend_configs WHERE listening = 1`)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	s.writeJSON(w, http.StatusOK, ids)
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
