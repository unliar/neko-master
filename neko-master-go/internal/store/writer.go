package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/clickhouse"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// Writer 负责把内存增量批量落库。
//
// SQLite 是唯一权威数据源；ch 为 nil 时（未启用 ClickHouse）
// 双写是 no-op。即使双写失败也不回滚 SQLite 事务。
type Writer struct {
	db  *DB
	ch  *clickhouse.Writer
	geo GeoResolver
}

// GeoResolver 按 IP 查 GeoIP，geo.Lookup 满足它。
// 定义在 store 包以避免 store -> collector 的反向依赖。
type GeoResolver interface {
	IP(ip string) *model.GeoIPInfo
}

// NewWriter 创建写入器。
func NewWriter(db *DB) *Writer { return &Writer{db: db} }

// AttachGeo 挂上 GeoIP 查询器，启用国家维度统计。nil 表示关闭。
func (w *Writer) AttachGeo(g GeoResolver) { w.geo = g }

// NewWriterWithClickHouse 创建带 ClickHouse 旁路双写的写入器。
func NewWriterWithClickHouse(db *DB, ch *clickhouse.Writer) *Writer {
	return &Writer{db: db, ch: ch}
}

// AttachClickHouse 挂上 ClickHouse 旁路写入器（nil 表示禁用）。
func (w *Writer) AttachClickHouse(ch *clickhouse.Writer) { w.ch = ch }

// ClickHouse 返回旁路写入器，供健康检查使用。
func (w *Writer) ClickHouse() *clickhouse.Writer { return w.ch }

// Flush 把一批增量写入所有维度表。
//
// 上游 batchUpdateTrafficStats 会把同一批数据按 11 种维度各自聚合成 Map
// 再统一 UPSERT，这里采用同样的策略。
func (w *Writer) Flush(backendID int64, updates []model.TrafficUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	tx, err := w.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)

	for _, u := range updates {
		ts := u.TimestampMs.UTC().Format("2006-01-02T15:04:05")
		tsFull := ts + "Z"
		if err := w.upsertOne(tx, backendID, u, tsFull, now); err != nil {
			return fmt.Errorf("flush update: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// ClickHouse 旁路双写：失败仅计数，绝不影响 SQLite 事务结果。
	if w.ch != nil {
		if err := w.ch.WriteTraffic(backendID, updates); err != nil {
			_ = err // 队列满等错误由 Writer 内部计数
		}
	}

	// 国家维度：把本轮流量按 IP 归属到国家，写 country_stats 三张表。
	// geo 未挂载（nil）时跳过。geo 查询走 Lookup.IP 自带的内存+DB 缓存，
	// 内网/回环 IP 直接返回 nil，不落国家维度（与上游一致：只有公网 IP 才有归属）。
	if w.geo != nil {
		if err := w.writeCountries(backendID, updates); err != nil {
			// 国家维度是附加维度，失败不应影响主流量已提交的结果
			_ = err
		}
	}

	return nil
}

// writeCountries 把一批流量按 IP 查 GeoIP 聚合成国家维度落库。
func (w *Writer) writeCountries(backendID int64, updates []model.TrafficUpdate) error {
	// 先按 IP 聚合本轮流量（同一 IP 多条增量合并，避免重复查 geo）
	type acc struct {
		up, down, conns int64
		ts              time.Time
	}
	byIP := map[string]*acc{}
	for _, u := range updates {
		if u.IP == "" {
			continue
		}
		a := byIP[u.IP]
		if a == nil {
			a = &acc{ts: u.TimestampMs}
			byIP[u.IP] = a
		}
		a.up += u.Upload
		a.down += u.Download
		a.conns += u.Connections
		if u.TimestampMs.After(a.ts) {
			a.ts = u.TimestampMs
		}
	}
	if len(byIP) == 0 {
		return nil
	}

	countries := make([]CountryUpdate, 0, len(byIP))
	for ip, a := range byIP {
		info := w.geo.IP(ip)
		if info == nil {
			continue // 内网/回环/未命中，无国家归属
		}
		countries = append(countries, geoToCountryUpdate(info, a.up, a.down, a.conns, a.ts))
	}
	if len(countries) == 0 {
		return nil
	}
	return w.FlushCountries(backendID, countries)
}

// upsertOne 把单条增量写入各个维度表。
func (w *Writer) upsertOne(tx *sql.Tx, be int64, u model.TrafficUpdate, ts, now string) error {
	minute := u.TimestampMs.UTC().Format("2006-01-02T15:04") + ":00"
	hour := u.TimestampMs.UTC().Format("2006-01-02T15") + ":00:00"

	domain := u.Domain
	if domain == "" {
		domain = u.IP // 无域名时退化到 IP，与上游一致
	}

	// ---- 时间桶：minute_stats / hourly_stats ----
	if err := exec(tx, `
		INSERT INTO minute_stats (backend_id, minute, upload, download, connections)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(backend_id, minute) DO UPDATE SET
			upload = upload + excluded.upload,
			download = download + excluded.download,
			connections = connections + excluded.connections`,
		be, minute, u.Upload, u.Download, u.Connections); err != nil {
		return err
	}

	if err := exec(tx, `
		INSERT INTO hourly_stats (backend_id, hour, upload, download, connections)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(backend_id, hour) DO UPDATE SET
			upload = upload + excluded.upload,
			download = download + excluded.download,
			connections = connections + excluded.connections`,
		be, hour, u.Upload, u.Download, u.Connections); err != nil {
		return err
	}

	// ---- 事实表：minute_dim_stats / hourly_dim_stats ----
	if err := exec(tx, `
		INSERT INTO minute_dim_stats
			(backend_id, minute, domain, ip, source_ip, chain, rule, upload, download, connections)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(backend_id, minute, domain, ip, source_ip, chain, rule) DO UPDATE SET
			upload = upload + excluded.upload,
			download = download + excluded.download,
			connections = connections + excluded.connections`,
		be, minute, domain, u.IP, u.SourceIP, u.FullChain, u.Rule,
		u.Upload, u.Download, u.Connections); err != nil {
		return err
	}

	if err := exec(tx, `
		INSERT INTO hourly_dim_stats
			(backend_id, hour, domain, ip, source_ip, chain, rule, upload, download, connections)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(backend_id, hour, domain, ip, source_ip, chain, rule) DO UPDATE SET
			upload = upload + excluded.upload,
			download = download + excluded.download,
			connections = connections + excluded.connections`,
		be, hour, domain, u.IP, u.SourceIP, u.FullChain, u.Rule,
		u.Upload, u.Download, u.Connections); err != nil {
		return err
	}

	// ---- 域名维度 ----
	if domain != "" {
		if err := exec(tx, `
			INSERT INTO domain_stats
				(backend_id, domain, ips, total_upload, total_download, total_connections, last_seen, rules, chains)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, domain) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = excluded.last_seen,
				ips = CASE
					WHEN domain_stats.ips IS NULL THEN excluded.ips
					WHEN LENGTH(domain_stats.ips) > 4000 THEN domain_stats.ips
					WHEN excluded.ips = '' THEN domain_stats.ips
					WHEN INSTR(',' || domain_stats.ips || ',', ',' || excluded.ips || ',') > 0 THEN domain_stats.ips
					ELSE domain_stats.ips || ',' || excluded.ips END,
				rules = CASE
					WHEN domain_stats.rules IS NULL THEN excluded.rules
					WHEN LENGTH(domain_stats.rules) > 4000 THEN domain_stats.rules
					WHEN excluded.rules = '' THEN domain_stats.rules
					WHEN INSTR(',' || domain_stats.rules || ',', ',' || excluded.rules || ',') > 0 THEN domain_stats.rules
					ELSE domain_stats.rules || ',' || excluded.rules END,
				chains = CASE
					WHEN domain_stats.chains IS NULL THEN excluded.chains
					WHEN LENGTH(domain_stats.chains) > 4000 THEN domain_stats.chains
					WHEN excluded.chains = '' THEN domain_stats.chains
					WHEN INSTR(',' || domain_stats.chains || ',', ',' || excluded.chains || ',') > 0 THEN domain_stats.chains
					ELSE domain_stats.chains || ',' || excluded.chains END`,
			be, domain, u.IP, u.Upload, u.Download, u.Connections, ts, u.Rule, u.FullChain); err != nil {
			return err
		}
	}

	// ---- IP 维度 ----
	if u.IP != "" {
		if err := exec(tx, `
			INSERT INTO ip_stats
				(backend_id, ip, domains, total_upload, total_download, total_connections, last_seen, chains, rules)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, ip) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = excluded.last_seen,
				domains = CASE
					WHEN ip_stats.domains IS NULL THEN excluded.domains
					WHEN LENGTH(ip_stats.domains) > 4000 THEN ip_stats.domains
					WHEN excluded.domains = '' THEN ip_stats.domains
					WHEN INSTR(',' || ip_stats.domains || ',', ',' || excluded.domains || ',') > 0 THEN ip_stats.domains
					ELSE ip_stats.domains || ',' || excluded.domains END,
				chains = CASE
					WHEN ip_stats.chains IS NULL THEN excluded.chains
					WHEN LENGTH(ip_stats.chains) > 4000 THEN ip_stats.chains
					WHEN excluded.chains = '' THEN ip_stats.chains
					WHEN INSTR(',' || ip_stats.chains || ',', ',' || excluded.chains || ',') > 0 THEN ip_stats.chains
					ELSE ip_stats.chains || ',' || excluded.chains END,
				rules = CASE
					WHEN ip_stats.rules IS NULL THEN excluded.rules
					WHEN LENGTH(ip_stats.rules) > 4000 THEN ip_stats.rules
					WHEN excluded.rules = '' THEN ip_stats.rules
					WHEN INSTR(',' || ip_stats.rules || ',', ',' || excluded.rules || ',') > 0 THEN ip_stats.rules
					ELSE ip_stats.rules || ',' || excluded.rules END`,
			be, u.IP, domain, u.Upload, u.Download, u.Connections, ts, u.FullChain, u.Rule); err != nil {
			return err
		}
	}

	// ---- 设备维度 ----
	if u.SourceIP != "" {
		if err := exec(tx, `
			INSERT INTO device_stats
				(backend_id, source_ip, total_upload, total_download, total_connections, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, source_ip) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = excluded.last_seen`,
			be, u.SourceIP, u.Upload, u.Download, u.Connections, ts); err != nil {
			return err
		}
		if domain != "" {
			if err := exec(tx, `
				INSERT INTO device_domain_stats
					(backend_id, source_ip, domain, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, source_ip, domain) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, u.SourceIP, domain, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
		if u.IP != "" {
			if err := exec(tx, `
				INSERT INTO device_ip_stats
					(backend_id, source_ip, ip, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, source_ip, ip) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, u.SourceIP, u.IP, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
	}

	// ---- 代理链路维度 ----
	if u.FullChain != "" {
		if err := exec(tx, `
			INSERT INTO proxy_stats
				(backend_id, chain, total_upload, total_download, total_connections, last_seen)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, chain) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = excluded.last_seen`,
			be, u.FullChain, u.Upload, u.Download, u.Connections, ts); err != nil {
			return err
		}
		if domain != "" {
			if err := exec(tx, `
				INSERT INTO domain_proxy_stats
					(backend_id, domain, chain, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, domain, chain) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, domain, u.FullChain, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
		if u.IP != "" {
			if err := exec(tx, `
				INSERT INTO ip_proxy_stats
					(backend_id, ip, chain, total_upload, total_download, total_connections, last_seen, domains)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, ip, chain) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen,
					domains = CASE
						WHEN ip_proxy_stats.domains IS NULL THEN excluded.domains
						WHEN LENGTH(ip_proxy_stats.domains) > 4000 THEN ip_proxy_stats.domains
						WHEN excluded.domains = '' THEN ip_proxy_stats.domains
						WHEN INSTR(',' || ip_proxy_stats.domains || ',', ',' || excluded.domains || ',') > 0 THEN ip_proxy_stats.domains
						ELSE ip_proxy_stats.domains || ',' || excluded.domains END`,
				be, u.IP, u.FullChain, u.Upload, u.Download, u.Connections, ts, domain); err != nil {
				return err
			}
		}
	}

	// ---- 规则维度 ----
	ruleName := buildRuleName(u.Rule, u.RulePayload)
	if ruleName != "" {
		if err := exec(tx, `
			INSERT INTO rule_stats
				(backend_id, rule, final_proxy, total_upload, total_download, total_connections, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, rule) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = excluded.last_seen,
				final_proxy = excluded.final_proxy`,
			be, ruleName, u.Chain, u.Upload, u.Download, u.Connections, ts); err != nil {
			return err
		}
		if err := exec(tx, `
			INSERT OR IGNORE INTO rule_proxy_map (backend_id, rule, proxy) VALUES (?, ?, ?)`,
			be, ruleName, u.Chain); err != nil {
			return err
		}
		if u.FullChain != "" {
			if err := exec(tx, `
				INSERT INTO rule_chain_traffic
					(backend_id, rule, chain, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, rule, chain) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, ruleName, u.FullChain, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
		if domain != "" {
			if err := exec(tx, `
				INSERT INTO rule_domain_traffic
					(backend_id, rule, domain, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, rule, domain) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, ruleName, domain, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
		if u.IP != "" {
			if err := exec(tx, `
				INSERT INTO rule_ip_traffic
					(backend_id, rule, ip, total_upload, total_download, total_connections, last_seen)
				VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(backend_id, rule, ip) DO UPDATE SET
					total_upload = total_upload + excluded.total_upload,
					total_download = total_download + excluded.total_download,
					total_connections = total_connections + excluded.total_connections,
					last_seen = excluded.last_seen`,
				be, ruleName, u.IP, u.Upload, u.Download, u.Connections, ts); err != nil {
				return err
			}
		}
	}

	return nil
}

// buildRuleName 构造规则聚合键。
//
// 上游 buildRuleName 的三个分支：
//  1. payload 为空 → 直接用 rule
//  2. payload 是内置关键字（MATCH/DIRECT/REJECT 等）→ 用 rule
//  3. 否则 → "rule(payload)"
func buildRuleName(rule, payload string) string {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return ""
	}
	p := strings.TrimSpace(payload)
	if p == "" {
		return rule
	}
	switch strings.ToUpper(rule) {
	case "MATCH", "FINAL", "DIRECT", "REJECT":
		return rule
	}
	return rule + "(" + p + ")"
}

func exec(tx *sql.Tx, q string, args ...any) error {
	_, err := tx.Exec(q, args...)
	return err
}
