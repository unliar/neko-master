package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
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

// handleDBStats 对应 GET /api/db/stats，对齐上游 config.controller.ts。
//
// 返回四个字段（前端 backend-config-dialog.tsx 直接消费）：
//
//	size                 = sqliteSize + clickhouseSize
//	sqliteSize           = page_count * page_size（上游 getDatabaseSize 同步）
//	clickhouseSize       = traffic_* + country_* 表体积和（CH 未启用时为 0）
//	totalConnectionsCount= CH 就绪时取 traffic_minute 行数，否则 SQLite minute_stats 行数
//
// 上游的命名有误导性：函数叫 getTotalConnectionLogsCount，
// 但查的其实是 minute_stats 而不是 connection_logs，这里照做。
//
// 注意：之前返回的 tables / dbPath / backendCount 三个字段前端不用，
// 而 totalConnectionsCount 缺失会让 formatNumber(undefined) 抛 TypeError 崩溃弹窗。
func (s *Server) handleDBStats(w http.ResponseWriter, r *http.Request) {
	sqliteSize := s.dbSizeBytes()

	// sqliteCount 对齐上游 getTotalConnectionLogsCount：查 minute_stats。
	var sqliteCount int64
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM minute_stats`).Scan(&sqliteCount)

	// ClickHouse 就绪时用它的数据（traffic_minute 行数与 traffic_*/country_* 体积），
	// 未启用或不健康时全部为 0，行为落回到 SQLite，与上游 resolveClickHouseStorageStats 一致。
	chSize, chRows := s.clickHouseStorageStats(r.Context())

	size := sqliteSize + chSize
	count := sqliteCount
	if chSize > 0 || chRows > 0 {
		count = chRows
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"size":                  size,
		"sqliteSize":            sqliteSize,
		"clickhouseSize":        chSize,
		"totalConnectionsCount": count,
	})
}

// dbSizeBytes 返回 SQLite 主库文件体积（含 -wal）。
//
// 上游用 page_count * page_size，不含 -wal/-shm。Go 侧这里也计算
// page_count * page_size 以保数值可比，但额外加上 WAL 体积：
// WAL 里存的是真实占用（默认上限上千页），只看主库会显式偏小。
func (s *Server) dbSizeBytes() int64 {
	var pages, pageSize int64
	if err := s.db.SQL().QueryRow(`PRAGMA page_count`).Scan(&pages); err != nil {
		return 0
	}
	if err := s.db.SQL().QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0
	}
	return pages*pageSize + walSize(s.db.Path())
}

// walSize 返回 -wal / -shm 文件体积，不存在时为 0。
func walSize(dbPath string) int64 {
	var total int64
	for _, suffix := range []string{"-wal", "-shm"} {
		if fi, err := os.Stat(dbPath + suffix); err == nil {
			total += fi.Size()
		}
	}
	return total
}

// clickHouseStorageStats 返回 ClickHouse 侧体积与 traffic_minute 行数。
//
// CH 未启用、不健康或查询失败时返回 (0, 0)，让调用方落回 SQLite。
// 这上溉 resolveClickHouseStorageStats 的容错行为：它抛异常时 chStats 为 null，
// 对应到 clickhouseSize=0 且 totalConnectionsCount 取 SQLite 值。
func (s *Server) clickHouseStorageStats(ctx context.Context) (sizeBytes, trafficRows int64) {
	if s.ch == nil || !s.ch.Enabled() {
		return 0, 0
	}
	var err error
	if sizeBytes, err = s.ch.StorageSizeBytes(ctx); err != nil {
		return 0, 0
	}
	if trafficRows, err = s.ch.TrafficMinuteRows(ctx); err != nil {
		return 0, 0
	}
	return sizeBytes, trafficRows
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
