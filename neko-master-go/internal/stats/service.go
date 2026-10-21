// Package stats 实现 /api/stats/* 的查询逻辑。
//
// 全部走 SQLite 预聚合表（不实现 ClickHouse 路径），
// 对外响应结构与 shared 类型严格一致。
package stats

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
	"github.com/unliar/neko-master/neko-master-go/internal/store"
)

// Service 提供统计查询。
type Service struct {
	db *store.DB
}

// New 创建服务。
func New(db *store.DB) *Service { return &Service{db: db} }

// splitList 把逗号分隔字段拆成数组（保持去重后的原顺序）。
func splitList(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

// DomainRow 是 domain_stats 的展开形态（对齐 shared DomainStats）。
type DomainRow struct {
	Domain           string   `json:"domain"`
	IPs              []string `json:"ips"`
	TotalUpload      int64    `json:"totalUpload"`
	TotalDownload    int64    `json:"totalDownload"`
	TotalConnections int64    `json:"totalConnections"`
	LastSeen         string   `json:"lastSeen"`
	Rules            []string `json:"rules"`
	Chains           []string `json:"chains"`
}

// IPRow 是 ip_stats 的展开形态（对齐 shared IPStats）。
type IPRow struct {
	IP               string   `json:"ip"`
	Domains          []string `json:"domains"`
	TotalUpload      int64    `json:"totalUpload"`
	TotalDownload    int64    `json:"totalDownload"`
	TotalConnections int64    `json:"totalConnections"`
	LastSeen         string   `json:"lastSeen"`
	ASN              string   `json:"asn,omitempty"`
	Chains           []string `json:"chains,omitempty"`
}

// ProxyRow 对齐 shared ProxyStats。
type ProxyRow struct {
	Chain            string `json:"chain"`
	TotalUpload      int64  `json:"totalUpload"`
	TotalDownload    int64  `json:"totalDownload"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen"`
}

// RuleRow 对齐 shared RuleStats。
type RuleRow struct {
	Rule             string `json:"rule"`
	FinalProxy       string `json:"finalProxy"`
	TotalUpload      int64  `json:"totalUpload"`
	TotalDownload    int64  `json:"totalDownload"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen"`
}

// DeviceRow 对齐 shared DeviceStats。
type DeviceRow struct {
	SourceIP         string `json:"sourceIP"`
	TotalUpload      int64  `json:"totalUpload"`
	TotalDownload    int64  `json:"totalDownload"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen"`
}

// HourlyRow 对齐 shared HourlyStats。
type HourlyRow struct {
	Hour        string `json:"hour"`
	Upload      int64  `json:"upload"`
	Download    int64  `json:"download"`
	Connections int64  `json:"connections"`
}

// CountryRow 对齐 shared CountryStats。
type CountryRow struct {
	Country          string `json:"country"`
	CountryName      string `json:"countryName"`
	Continent        string `json:"continent"`
	TotalUpload      int64  `json:"totalUpload"`
	TotalDownload    int64  `json:"totalDownload"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
}

// SummaryResponse 对齐 stats.types.ts 的 SummaryResponse。
type SummaryResponse struct {
	Backend          BackendInfo `json:"backend"`
	TotalConnections int64       `json:"totalConnections"`
	TotalUpload      int64       `json:"totalUpload"`
	TotalDownload    int64       `json:"totalDownload"`
	TotalDomains     int64       `json:"totalDomains"`
	TotalIPs         int64       `json:"totalIPs"`
	TotalRules       int64       `json:"totalRules"`
	TotalProxies     int64       `json:"totalProxies"`
	TodayUpload      int64       `json:"todayUpload"`
	TodayDownload    int64       `json:"todayDownload"`
	TopDomains       []DomainRow `json:"topDomains"`
	TopIPs           []IPRow     `json:"topIPs"`
	ProxyStats       []ProxyRow  `json:"proxyStats"`
	RuleStats        []RuleRow   `json:"ruleStats"`
	HourlyStats      []HourlyRow `json:"hourlyStats"`
}

// BackendInfo 是 summary 里嵌的 backend 概要。
type BackendInfo struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IsActive  bool   `json:"isActive"`
	Listening bool   `json:"listening"`
}

// GlobalSummary 对齐 stats.types.ts 的 GlobalSummary。
type GlobalSummary struct {
	TotalConnections int64 `json:"totalConnections"`
	TotalUpload      int64 `json:"totalUpload"`
	TotalDownload    int64 `json:"totalDownload"`
	UniqueDomains    int64 `json:"uniqueDomains"`
	UniqueIPs        int64 `json:"uniqueIPs"`
	BackendCount     int64 `json:"backendCount"`
}

// GetBackend 读取单个 backend。
func (s *Service) GetBackend(id int64) (*model.Backend, error) {
	row := s.db.SQL().QueryRow(`
		SELECT id, name, url, token, type, enabled, is_active, listening
		FROM backend_configs WHERE id = ?`, id)
	var b model.Backend
	var enabled, isActive, listening int
	if err := row.Scan(&b.ID, &b.Name, &b.URL, &b.Token, &b.Type, &enabled, &isActive, &listening); err != nil {
		return nil, err
	}
	b.Enabled = enabled != 0
	b.IsActive = isActive != 0
	b.Listening = listening != 0
	return &b, nil
}

// ActiveBackendID 返回当前激活 backend 的 id；没有则返回 0。
func (s *Service) ActiveBackendID() (int64, error) {
	var id int64
	err := s.db.SQL().QueryRow(`SELECT id FROM backend_configs WHERE is_active = 1 LIMIT 1`).Scan(&id)
	if err == sql.ErrNoRows {
		// 回退到第一个启用的
		err = s.db.SQL().QueryRow(`SELECT id FROM backend_configs WHERE enabled = 1 ORDER BY id LIMIT 1`).Scan(&id)
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Summary 聚合总览。
func (s *Service) Summary(backendID int64) (*SummaryResponse, error) {
	be, err := s.GetBackend(backendID)
	if err != nil {
		return nil, fmt.Errorf("backend %d: %w", backendID, err)
	}

	out := &SummaryResponse{
		Backend: BackendInfo{
			ID:        be.ID,
			Name:      be.Name,
			IsActive:  be.IsActive,
			Listening: be.Listening,
		},
	}

	// 总计：从 proxy_stats 与 minute_stats 取
	if err := s.db.SQL().QueryRow(`
		SELECT COALESCE(SUM(total_upload),0), COALESCE(SUM(total_download),0),
		       COALESCE(SUM(total_connections),0)
		FROM proxy_stats WHERE backend_id = ?`, backendID).
		Scan(&out.TotalUpload, &out.TotalDownload, &out.TotalConnections); err != nil {
		return nil, err
	}

	// 唯一域名/IP 数
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM domain_stats WHERE backend_id = ?`, backendID).Scan(&out.TotalDomains)
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM ip_stats WHERE backend_id = ?`, backendID).Scan(&out.TotalIPs)

	// Top 域名 / IP
	if out.TopDomains, err = s.TopDomains(backendID, 10, 0); err != nil {
		return nil, err
	}
	if out.TopIPs, err = s.TopIPs(backendID, 10, 0); err != nil {
		return nil, err
	}

	// 全量代理 / 规则
	if out.ProxyStats, err = s.AllProxies(backendID); err != nil {
		return nil, err
	}
	if out.RuleStats, err = s.AllRules(backendID); err != nil {
		return nil, err
	}
	out.TotalProxies = int64(len(out.ProxyStats))
	out.TotalRules = int64(len(out.RuleStats))

	// 最近 24 小时
	if out.HourlyStats, err = s.Hourly(backendID, 24); err != nil {
		return nil, err
	}

	// 今日流量（UTC 当日）
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	if err := s.db.SQL().QueryRow(`
		SELECT COALESCE(SUM(upload),0), COALESCE(SUM(download),0)
		FROM minute_stats WHERE backend_id = ? AND minute >= ?`,
		backendID, todayStart.Format("2006-01-02T15:04")+":00").
		Scan(&out.TodayUpload, &out.TodayDownload); err != nil {
		return nil, err
	}

	return out, nil
}

// TopDomains 返回按流量降序的域名排行。
func (s *Service) TopDomains(backendID int64, limit, offset int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.SQL().Query(`
		SELECT domain, COALESCE(ips,''), total_upload, total_download, total_connections,
		       COALESCE(last_seen,''), COALESCE(rules,''), COALESCE(chains,'')
		FROM domain_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, domain
		LIMIT ? OFFSET ?`, backendID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DomainRow, 0, limit)
	for rows.Next() {
		var d DomainRow
		var ips, rules, chains string
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &rules, &chains); err != nil {
			return nil, err
		}
		d.IPs = splitList(ips)
		d.Rules = splitList(rules)
		d.Chains = splitList(chains)
		out = append(out, d)
	}
	return out, rows.Err()
}

// TopIPs 返回按流量降序的 IP 排行。
func (s *Service) TopIPs(backendID int64, limit, offset int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.SQL().Query(`
		SELECT ip, COALESCE(domains,''), total_upload, total_download, total_connections,
		       COALESCE(last_seen,''), COALESCE(asn,''), COALESCE(chains,'')
		FROM ip_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, ip
		LIMIT ? OFFSET ?`, backendID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]IPRow, 0, limit)
	for rows.Next() {
		var r IPRow
		var domains, chains string
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &chains); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains)
		r.Chains = splitList(chains)
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllProxies 返回全部代理链路统计。
func (s *Service) AllProxies(backendID int64) ([]ProxyRow, error) {
	rows, err := s.db.SQL().Query(`
		SELECT chain, total_upload, total_download, total_connections, COALESCE(last_seen,'')
		FROM proxy_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, chain`, backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProxyRow{}
	for rows.Next() {
		var r ProxyRow
		if err := rows.Scan(&r.Chain, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AllRules 返回全部规则统计。
func (s *Service) AllRules(backendID int64) ([]RuleRow, error) {
	rows, err := s.db.SQL().Query(`
		SELECT rule, COALESCE(final_proxy,''), total_upload, total_download,
		       total_connections, COALESCE(last_seen,'')
		FROM rule_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, rule`, backendID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []RuleRow{}
	for rows.Next() {
		var r RuleRow
		if err := rows.Scan(&r.Rule, &r.FinalProxy, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Hourly 返回最近 n 个小时的流量。
func (s *Service) Hourly(backendID int64, n int) ([]HourlyRow, error) {
	if n <= 0 {
		n = 24
	}
	rows, err := s.db.SQL().Query(`
		SELECT hour, upload, download, connections FROM hourly_stats
		WHERE backend_id = ?
		ORDER BY hour DESC LIMIT ?`, backendID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []HourlyRow{}
	for rows.Next() {
		var r HourlyRow
		if err := rows.Scan(&r.Hour, &r.Upload, &r.Download, &r.Connections); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 反转成时间升序，前端图表按升序绘制
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Devices 返回设备排行。
func (s *Service) Devices(backendID int64, limit, offset int) ([]DeviceRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.SQL().Query(`
		SELECT source_ip, total_upload, total_download, total_connections, COALESCE(last_seen,'')
		FROM device_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, source_ip
		LIMIT ? OFFSET ?`, backendID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DeviceRow{}
	for rows.Next() {
		var r DeviceRow
		if err := rows.Scan(&r.SourceIP, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Countries 返回国家排行。
func (s *Service) Countries(backendID int64, limit, offset int) ([]CountryRow, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.SQL().Query(`
		SELECT country, COALESCE(country_name,''), COALESCE(continent,''),
		       total_upload, total_download, total_connections, COALESCE(last_seen,'')
		FROM country_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC, country
		LIMIT ? OFFSET ?`, backendID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []CountryRow{}
	for rows.Next() {
		var r CountryRow
		if err := rows.Scan(&r.Country, &r.CountryName, &r.Continent,
			&r.TotalUpload, &r.TotalDownload, &r.TotalConnections, &r.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Global 返回跨所有 backend 的全局汇总。
func (s *Service) Global() (*GlobalSummary, error) {
	out := &GlobalSummary{}

	_ = s.db.SQL().QueryRow(`
		SELECT COALESCE(SUM(total_upload),0), COALESCE(SUM(total_download),0),
		       COALESCE(SUM(total_connections),0)
		FROM proxy_stats`).Scan(&out.TotalUpload, &out.TotalDownload, &out.TotalConnections)

	_ = s.db.SQL().QueryRow(`SELECT COUNT(DISTINCT domain) FROM domain_stats`).Scan(&out.UniqueDomains)
	_ = s.db.SQL().QueryRow(`SELECT COUNT(DISTINCT ip) FROM ip_stats`).Scan(&out.UniqueIPs)
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs`).Scan(&out.BackendCount)

	return out, nil
}

// Trend 返回时间序列。
//
// rangeMs 决定用分钟表还是小时表：> 6h 用小时聚合。
func (s *Service) Trend(backendID int64, start, end time.Time) ([]model.TrendPoint, error) {
	rangeMs := end.Sub(start).Milliseconds()
	useHourly := rangeMs > 6*3600*1000

	var q string
	if useHourly {
		q = `SELECT hour AS ts, upload, download FROM hourly_stats
		     WHERE backend_id = ? AND hour >= ? AND hour <= ?
		     ORDER BY hour`
	} else {
		q = `SELECT minute AS ts, upload, download FROM minute_stats
		     WHERE backend_id = ? AND minute >= ? AND minute <= ?
		     ORDER BY minute`
	}

	var startKey, endKey string
	if useHourly {
		startKey = start.UTC().Format("2006-01-02T15") + ":00:00"
		endKey = end.UTC().Format("2006-01-02T15") + ":00:00"
	} else {
		startKey = start.UTC().Format("2006-01-02T15:04") + ":00"
		endKey = end.UTC().Format("2006-01-02T15:04") + ":00"
	}

	rows, err := s.db.SQL().Query(q, backendID, startKey, endKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.TrendPoint{}
	for rows.Next() {
		var p model.TrendPoint
		if err := rows.Scan(&p.Timestamp, &p.Upload, &p.Download); err != nil {
			return nil, err
		}
		p.Total = p.Upload + p.Download
		out = append(out, p)
	}
	return out, rows.Err()
}
