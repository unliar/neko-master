package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// registerDB 注册数据库运维路由。
func (s *Server) registerDB() {
	s.mux.HandleFunc("GET /api/db/stats", s.handleDBStats)
	s.mux.HandleFunc("GET /api/db/retention", s.handleDBRetentionGet)
	s.mux.HandleFunc("PUT /api/db/retention", s.handleDBRetentionPut)
	s.mux.HandleFunc("GET /api/db/geoip", s.handleDBGeoIPGet)
	s.mux.HandleFunc("PUT /api/db/geoip", s.handleDBGeoIPPut)
	s.mux.HandleFunc("POST /api/db/cleanup", s.handleDBCleanup)
	s.mux.HandleFunc("POST /api/db/vacuum", s.handleDBVacuum)
}

// ---------------------------------------------------------------- stats

func (s *Server) handleDBStats(w http.ResponseWriter, r *http.Request) {
	tables := []string{
		"connection_logs", "domain_stats", "ip_stats", "proxy_stats", "rule_stats",
		"country_stats", "device_stats", "device_domain_stats", "device_ip_stats",
		"hourly_stats", "minute_stats", "minute_dim_stats", "hourly_dim_stats",
		"minute_country_stats", "hourly_country_stats", "domain_proxy_stats",
		"ip_proxy_stats", "rule_chain_traffic", "rule_domain_traffic",
		"rule_ip_traffic", "geoip_cache", "asn_cache",
	}

	counts := map[string]int64{}
	for _, t := range tables {
		var n int64
		if err := s.db.SQL().QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err == nil {
			counts[t] = n
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"tables":       counts,
		"dbPath":       s.db.Path(),
		"backendCount": s.backendCount(),
	})
}

func (s *Server) backendCount() int64 {
	var n int64
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs`).Scan(&n)
	return n
}

// ---------------------------------------------------------------- retention

type retentionCfg struct {
	ConnectionLogsDays int  `json:"connectionLogsDays"`
	HourlyStatsDays    int  `json:"hourlyStatsDays"`
	AutoCleanup        bool `json:"autoCleanup"`
}

func (s *Server) loadRetention() retentionCfg {
	c := retentionCfg{ConnectionLogsDays: 7, HourlyStatsDays: 30, AutoCleanup: true}
	var v string
	if err := s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'retention.connection_logs_days'`).Scan(&v); err == nil {
		c.ConnectionLogsDays = atoiDefault(v, 7)
	}
	if err := s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'retention.hourly_stats_days'`).Scan(&v); err == nil {
		c.HourlyStatsDays = atoiDefault(v, 30)
	}
	if err := s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'retention.auto_cleanup'`).Scan(&v); err == nil {
		c.AutoCleanup = v == "1"
	}
	return c
}

func (s *Server) setConfig(key, value string) {
	_, _ = s.db.SQL().Exec(`
		INSERT INTO app_config (key, value, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = CURRENT_TIMESTAMP`,
		key, value)
}

func (s *Server) handleDBRetentionGet(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, http.StatusOK, s.loadRetention())
}

func (s *Server) handleDBRetentionPut(w http.ResponseWriter, r *http.Request) {
	var req retentionCfg
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if req.ConnectionLogsDays <= 0 || req.HourlyStatsDays <= 0 {
		s.writeErr(w, http.StatusBadRequest, "Days must be positive")
		return
	}
	s.setConfig("retention.connection_logs_days", itoa(int64(req.ConnectionLogsDays)))
	s.setConfig("retention.hourly_stats_days", itoa(int64(req.HourlyStatsDays)))
	s.setConfig("retention.auto_cleanup", boolStr(req.AutoCleanup))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"message":   "Retention updated",
		"retention": s.loadRetention(),
	})
}

// ---------------------------------------------------------------- geoip

// geoCfg 对齐前端 api.ts 的 GeoLookupConfig。
//
// 注意 localMmdbReady / missingMmdbFiles / mmdbDir 三个字段
// 前端 backend-config-dialog.tsx 直接消费（包括
// missingMmdbFiles.join()），缺任何一个都会抛
// "Cannot read properties of undefined" 并崩溃整个对话框。
type geoCfg struct {
	Provider          string   `json:"provider"`
	APIURL            string   `json:"apiUrl"`
	ConfiguredProv    string   `json:"configuredProvider"`
	EffectiveProvider string   `json:"effectiveProvider"`
	MmdbDir           string   `json:"mmdbDir"`
	LocalMmdbReady    bool     `json:"localMmdbReady"`
	MissingMmdbFiles  []string `json:"missingMmdbFiles"`
}

func (s *Server) handleDBGeoIPGet(w http.ResponseWriter, r *http.Request) {
	var provider, apiURL string
	_ = s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'geoip.lookup_provider'`).Scan(&provider)
	_ = s.db.SQL().QueryRow(`SELECT value FROM app_config WHERE key = 'geoip.online_api_url'`).Scan(&apiURL)

	// configuredProvider 记用户显式配的值（否则回落环境默认）
	configured := provider
	if configured == "" {
		configured = s.cfg.GeoLookupProvider
	}
	if apiURL == "" {
		apiURL = s.cfg.GeoOnlineAPIURL
	}

	// MMDB 状态实时探文件，不读进程启动时的加载结果。
	// 否则用户拷进 MMDB 后仍会看到"缺文件"。
	missing := s.geo.MissingMMDBFiles()
	ready := len(missing) == 0

	// effectiveProvider 对齐上游 toGeoLookupResponse：
	// local 且未就绪 → 落回 online（用 !== false 的三目运算语义）。
	effective := configured
	if configured == "local" && !ready {
		effective = "online"
	}

	s.writeJSON(w, http.StatusOK, geoCfg{
		Provider:          effective,
		APIURL:            apiURL,
		ConfiguredProv:    configured,
		EffectiveProvider: effective,
		MmdbDir:           s.geo.MmdbDir(),
		LocalMmdbReady:    ready,
		MissingMmdbFiles:  missing,
	})
}

// handleDBGeoIPPut 对应 PUT /api/db/geoip，对齐上游 config.controller.ts。
//
// 校验顺序必须与上游一致：
//  1. provider 非 online/local → 400 provider must be 'online' or 'local'
//  2. onlineApiUrl 提供了但不是 http/https → 400
//  3. provider == local 且 MMDB 未就绪 → 400，带 missingMmdbFiles
//
// 第 3 条是关键：上游不允许在没有 MMDB 时切到
// local（否则 GeoIP 全面失效），我们之前只打 warn
// 然后照单，行为与上游不一致。
func (s *Server) handleDBGeoIPPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider     string `json:"provider"`
		OnlineAPIURL string `json:"onlineApiUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	if req.Provider != "" && req.Provider != "online" && req.Provider != "local" {
		s.writeErr(w, http.StatusBadRequest, "provider must be 'online' or 'local'")
		return
	}

	if req.OnlineAPIURL != "" && !isValidHTTPURL(req.OnlineAPIURL) {
		s.writeErr(w, http.StatusBadRequest, "onlineApiUrl must be a valid http/https URL")
		return
	}

	p := req.Provider
	apiURL := req.OnlineAPIURL
	if p == "" {
		p = s.cfg.GeoLookupProvider
	}
	if apiURL == "" {
		apiURL = s.cfg.GeoOnlineAPIURL
	}

	// provider=local 前置校验：拒绝在 MMDB 缺失时切到 local。
	// 这里直接探文件，不依赖进程启动时的加载结果。
	missing := s.geo.MissingMMDBFiles()
	if p == "local" && len(missing) > 0 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":            "Local MMDB is not ready. Missing required files.",
			"missingMmdbFiles": missing,
		})
		return
	}

	s.setConfig("geoip.lookup_provider", p)
	s.setConfig("geoip.online_api_url", apiURL)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"message": "GeoIP configuration updated",
		"config": geoCfg{
			Provider:          p,
			APIURL:            apiURL,
			ConfiguredProv:    p,
			EffectiveProvider: p,
			MmdbDir:           s.geo.MmdbDir(),
			LocalMmdbReady:    len(missing) == 0,
			MissingMmdbFiles:  missing,
		},
	})
}

// isValidHTTPURL 对齐上游 isValidHttpUrl：只接受 http/https。
func isValidHTTPURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

// ---------------------------------------------------------------- cleanup

func (s *Server) handleDBCleanup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Days      *int   `json:"days"`
		BackendID *int64 `json:"backendId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	c := s.loadRetention()
	days := c.ConnectionLogsDays
	if req.Days != nil && *req.Days > 0 {
		days = *req.Days
	}

	cutoffMinute := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02T15:04") + ":00"

	total := int64(0)

	// 原始日志与分钟桶按 connection_logs_days 清理
	minuteTables := []string{
		"connection_logs", "minute_stats", "minute_dim_stats",
		"minute_country_stats", "backend_health_logs",
	}
	for _, t := range minuteTables {
		tsCol := "timestamp"
		if t == "minute_stats" || t == "minute_dim_stats" || t == "minute_country_stats" {
			tsCol = "minute"
		} else if t == "backend_health_logs" {
			tsCol = "minute"
		}
		q := `DELETE FROM ` + t + ` WHERE ` + tsCol + ` < ?`
		if req.BackendID != nil {
			q += ` AND backend_id = ?`
			if res, err := s.db.SQL().Exec(q, cutoffMinute, *req.BackendID); err == nil {
				n, _ := res.RowsAffected()
				total += n
			}
			continue
		}
		if res, err := s.db.SQL().Exec(q, cutoffMinute); err == nil {
			n, _ := res.RowsAffected()
			total += n
		}
	}

	// 小时桶按 hourly_stats_days 清理
	hourCutoff := time.Now().UTC().AddDate(0, 0, -c.HourlyStatsDays)
	hourKey := hourCutoff.Format("2006-01-02T15") + ":00:00"
	for _, t := range []string{"hourly_stats", "hourly_dim_stats", "hourly_country_stats"} {
		q := `DELETE FROM ` + t + ` WHERE hour < ?`
		if req.BackendID != nil {
			q += ` AND backend_id = ?`
			if res, err := s.db.SQL().Exec(q, hourKey, *req.BackendID); err == nil {
				n, _ := res.RowsAffected()
				total += n
			}
			continue
		}
		if res, err := s.db.SQL().Exec(q, hourKey); err == nil {
			n, _ := res.RowsAffected()
			total += n
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"message": "Cleanup completed",
		"deleted": total,
	})
}

// ---------------------------------------------------------------- vacuum

func (s *Server) handleDBVacuum(w http.ResponseWriter, r *http.Request) {
	if _, err := s.db.SQL().Exec(`VACUUM`); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Database vacuumed"})
}

// ---------------------------------------------------------------- 工具

func atoiDefault(s string, def int) int {
	n := 0
	ok := s != ""
	for _, c := range s {
		if c < '0' || c > '9' {
			ok = false
			break
		}
		n = n*10 + int(c-'0')
	}
	if !ok {
		return def
	}
	return n
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
