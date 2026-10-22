package api

import (
	"encoding/json"
	"net/http"
	"strings"
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

	// effectiveProvider 是真正生效的值：配了 local 但 MMDB
	// 没加载成功时仍然回落到 online，必须告诉前端。
	dir, ready, missing := s.geo.LocalStatus()
	effective := configured
	if configured == "local" && !ready {
		effective = "online"
	}

	s.writeJSON(w, http.StatusOK, geoCfg{
		Provider:          effective,
		APIURL:            apiURL,
		ConfiguredProv:    configured,
		EffectiveProvider: effective,
		MmdbDir:           dir,
		LocalMmdbReady:    ready,
		MissingMmdbFiles:  missing,
	})
}

func (s *Server) handleDBGeoIPPut(w http.ResponseWriter, r *http.Request) {
	var req geoCfg
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	p := strings.ToLower(strings.TrimSpace(req.Provider))
	if p != "online" && p != "local" {
		s.writeErr(w, http.StatusBadRequest, "provider must be 'online' or 'local'")
		return
	}
	if req.APIURL == "" {
		req.APIURL = s.cfg.GeoOnlineAPIURL
	}
	s.setConfig("geoip.lookup_provider", p)
	s.setConfig("geoip.online_api_url", req.APIURL)
	// 返回完整 geoCfg，与 GET 保持同一套字段。
	// 前端改完配置会用这份数据更新表单，
	// 缺 localMmdbReady / missingMmdbFiles 依然会抛 TypeError。
	// 注意 MMDB 的加载状态不会因本次 PUT 改变，照实返回。
	dir, ready, missing := s.geo.LocalStatus()
	if p == "local" && !ready {
		s.log.Warn("geoip: provider set to local but MMDB not loaded, falls back to online",
			"dir", dir, "missing", missing)
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"message": "GeoIP settings updated",
		"config": geoCfg{
			Provider:          p,
			APIURL:            req.APIURL,
			ConfiguredProv:    p,
			EffectiveProvider: p,
			MmdbDir:           dir,
			LocalMmdbReady:    ready,
			MissingMmdbFiles:  missing,
		},
	})
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
