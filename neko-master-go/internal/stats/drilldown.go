package stats

import (
	"database/sql"
)

// ProxyTraffic 对齐 shared ProxyTrafficStats：
// 给定域名/IP，按出口链拆分流量。
type ProxyTraffic struct {
	Chain            string `json:"chain"`
	TotalUpload      int64  `json:"totalUpload"`
	TotalDownload    int64  `json:"totalDownload"`
	TotalConnections int64  `json:"totalConnections"`
}

// ---------------------------------------------------------------- 域名下钻

// DomainProxyStats 对应 GET /api/stats/domains/proxy-stats。
// chain 必填。
func (s *Service) DomainProxyStats(backendID int64, domain, sourceIP, sourceChain string, limit int) ([]ProxyTraffic, error) {
	if limit <= 0 {
		limit = 5000
	}
	out := []ProxyTraffic{}

	q := `SELECT chain, total_upload, total_download, total_connections
	      FROM domain_proxy_stats
	      WHERE backend_id = ? AND domain = ?`
	args := []any{backendID, domain}
	if sourceChain != "" {
		q += ` AND chain = ?`
		args = append(args, sourceChain)
	}
	q += ` ORDER BY (total_upload + total_download) DESC, chain LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r ProxyTraffic
		if err := rows.Scan(&r.Chain, &r.TotalUpload, &r.TotalDownload, &r.TotalConnections); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DomainIPDetails 对应 GET /api/stats/domains/ip-details。
// domain 必填；返回该域名关联过的 IP 及其流量。
func (s *Service) DomainIPDetails(backendID int64, domain, sourceIP, sourceChain string, limit int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT ip, COALESCE(domains,''), total_upload, total_download,
	             total_connections, COALESCE(last_seen,''), COALESCE(asn,''), COALESCE(chains,'')
	      FROM ip_stats
	      WHERE backend_id = ?
	        AND (',' || COALESCE(domains,'') || ',') LIKE ('%,' || ? || ',%')`
	args := []any{backendID, domain}
	q += ` ORDER BY (total_upload + total_download) DESC, ip LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRow{}
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

// ---------------------------------------------------------------- IP 下钻

// IPProxyStats 对应 GET /api/stats/ips/proxy-stats。ip 必填。
func (s *Service) IPProxyStats(backendID int64, ip, sourceIP, sourceChain string, limit int) ([]ProxyTraffic, error) {
	if limit <= 0 {
		limit = 5000
	}

	q := `SELECT chain, total_upload, total_download, total_connections
	      FROM ip_proxy_stats
	      WHERE backend_id = ? AND ip = ?`
	args := []any{backendID, ip}
	if sourceChain != "" {
		q += ` AND chain = ?`
		args = append(args, sourceChain)
	}
	q += ` ORDER BY (total_upload + total_download) DESC, chain LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProxyTraffic{}
	for rows.Next() {
		var r ProxyTraffic
		if err := rows.Scan(&r.Chain, &r.TotalUpload, &r.TotalDownload, &r.TotalConnections); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IPDomainDetails 对应 GET /api/stats/ips/domain-details。ip 必填。
func (s *Service) IPDomainDetails(backendID int64, ip, sourceIP, sourceChain string, limit int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 100
	}

	q := `SELECT domain, COALESCE(ips,''), total_upload, total_download,
	             total_connections, COALESCE(last_seen,''), COALESCE(rules,''), COALESCE(chains,'')
	      FROM domain_stats
	      WHERE backend_id = ?
	        AND (',' || COALESCE(ips,'') || ',') LIKE ('%,' || ? || ',%')`
	args := []any{backendID, ip}
	q += ` ORDER BY (total_upload + total_download) DESC, domain LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DomainRow{}
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

// ---------------------------------------------------------------- 代理链下钻

// ProxyDomains 对应 GET /api/stats/proxies/domains。chain 必填。
func (s *Service) ProxyDomains(backendID int64, chain string, limit int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 5000
	}

	rows, err := s.db.SQL().Query(`
		SELECT d.domain, COALESCE(d.ips,''), d.total_upload, d.total_download,
		       d.total_connections, COALESCE(d.last_seen,''), COALESCE(d.rules,''),
		       COALESCE(dp.chain,'')
		FROM domain_proxy_stats dp
		JOIN domain_stats d
		  ON d.backend_id = dp.backend_id AND d.domain = dp.domain
		WHERE dp.backend_id = ? AND dp.chain = ? AND dp.domain != ''
		ORDER BY (dp.total_upload + dp.total_download) DESC, d.domain
		LIMIT ?`, backendID, chain, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DomainRow{}
	for rows.Next() {
		var d DomainRow
		var ips, rules, ch string
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &rules, &ch); err != nil {
			return nil, err
		}
		d.IPs = splitList(ips)
		d.Rules = splitList(rules)
		if ch != "" {
			d.Chains = []string{ch}
		} else {
			d.Chains = []string{}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ProxyIPs 对应 GET /api/stats/proxies/ips。chain 必填。
func (s *Service) ProxyIPs(backendID int64, chain string, limit int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 5000
	}

	rows, err := s.db.SQL().Query(`
		SELECT i.ip, COALESCE(i.domains,''), i.total_upload, i.total_download,
		       i.total_connections, COALESCE(i.last_seen,''), COALESCE(i.asn,''),
		       COALESCE(ip.chain,'')
		FROM ip_proxy_stats ip
		JOIN ip_stats i
		  ON i.backend_id = ip.backend_id AND i.ip = ip.ip
		WHERE ip.backend_id = ? AND ip.chain = ?
		ORDER BY (ip.total_upload + ip.total_download) DESC, i.ip
		LIMIT ?`, backendID, chain, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRow{}
	for rows.Next() {
		var r IPRow
		var domains, ch string
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &ch); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains)
		if ch != "" {
			r.Chains = []string{ch}
		} else {
			r.Chains = []string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 规则下钻

// RuleDomains 对应 GET /api/stats/rules/domains。rule 必填。
// 注意：上游把 rules 字段硬编码为 [rule] 单元素。
func (s *Service) RuleDomains(backendID int64, rule string, limit int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 5000
	}

	rows, err := s.db.SQL().Query(`
		SELECT rd.domain, COALESCE(d.ips,''), rd.total_upload, rd.total_download,
		       rd.total_connections, COALESCE(rd.last_seen,''), COALESCE(d.chains,'')
		FROM rule_domain_traffic rd
		LEFT JOIN domain_stats d
		  ON d.backend_id = rd.backend_id AND d.domain = rd.domain
		WHERE rd.backend_id = ? AND rd.rule = ?
		ORDER BY (rd.total_upload + rd.total_download) DESC, rd.domain
		LIMIT ?`, backendID, rule, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DomainRow{}
	for rows.Next() {
		var d DomainRow
		var ips, chains string
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &chains); err != nil {
			return nil, err
		}
		d.IPs = splitList(ips)
		d.Rules = []string{rule} // 上游硬编码
		d.Chains = splitList(chains)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RuleIPs 对应 GET /api/stats/rules/ips。rule 必填。
func (s *Service) RuleIPs(backendID int64, rule string, limit int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 5000
	}

	rows, err := s.db.SQL().Query(`
		SELECT rt.ip, COALESCE(i.domains,''), rt.total_upload, rt.total_download,
		       rt.total_connections, COALESCE(rt.last_seen,''), COALESCE(i.asn,''),
		       COALESCE(i.chains,'')
		FROM rule_ip_traffic rt
		LEFT JOIN ip_stats i
		  ON i.backend_id = rt.backend_id AND i.ip = rt.ip
		WHERE rt.backend_id = ? AND rt.rule = ?
		ORDER BY (rt.total_upload + rt.total_download) DESC, rt.ip
		LIMIT ?`, backendID, rule, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRow{}
	for rows.Next() {
		var r IPRow
		var domains, chains sql.NullString
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &chains); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains.String)
		r.Chains = splitList(chains.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleDomainProxyStats 对应 GET /api/stats/rules/domains/proxy-stats。
//
// 复刻上游行为：无时间范围时直接返回空数组（源码级不一致，见 API-DETAILS §1.16）。
func (s *Service) RuleDomainProxyStats(backendID int64, rule, domain string, hasRange bool) ([]ProxyTraffic, error) {
	if !hasRange {
		return []ProxyTraffic{}, nil
	}
	// 有时间范围时从事实表聚合
	rows, err := s.db.SQL().Query(`
		SELECT chain, SUM(upload), SUM(download), SUM(connections)
		FROM minute_dim_stats
		WHERE backend_id = ? AND rule = ? AND domain = ?
		GROUP BY chain
		ORDER BY (SUM(upload) + SUM(download)) DESC`, backendID, rule, domain)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProxyTraffic{}
	for rows.Next() {
		var r ProxyTraffic
		if err := rows.Scan(&r.Chain, &r.TotalUpload, &r.TotalDownload, &r.TotalConnections); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleIPProxyStats 对应 GET /api/stats/rules/ips/proxy-stats。行为同上。
func (s *Service) RuleIPProxyStats(backendID int64, rule, ip string, hasRange bool) ([]ProxyTraffic, error) {
	if !hasRange {
		return []ProxyTraffic{}, nil
	}
	rows, err := s.db.SQL().Query(`
		SELECT chain, SUM(upload), SUM(download), SUM(connections)
		FROM minute_dim_stats
		WHERE backend_id = ? AND rule = ? AND ip = ?
		GROUP BY chain
		ORDER BY (SUM(upload) + SUM(download)) DESC`, backendID, rule, ip)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ProxyTraffic{}
	for rows.Next() {
		var r ProxyTraffic
		if err := rows.Scan(&r.Chain, &r.TotalUpload, &r.TotalDownload, &r.TotalConnections); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleDomainIPDetails 对应 GET /api/stats/rules/domains/ip-details。
func (s *Service) RuleDomainIPDetails(backendID int64, rule, domain string, limit int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.SQL().Query(`
		SELECT rt.ip, COALESCE(i.domains,''), rt.total_upload, rt.total_download,
		       rt.total_connections, COALESCE(rt.last_seen,''), COALESCE(i.asn,''),
		       COALESCE(i.chains,'')
		FROM rule_ip_traffic rt
		LEFT JOIN ip_stats i
		  ON i.backend_id = rt.backend_id AND i.ip = rt.ip
		WHERE rt.backend_id = ? AND rt.rule = ?
		  AND rt.ip IN (
		    SELECT ip FROM rule_ip_traffic
		    WHERE backend_id = ? AND rule = ?
		  )
		ORDER BY (rt.total_upload + rt.total_download) DESC
		LIMIT ?`, backendID, rule, backendID, rule, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRow{}
	for rows.Next() {
		var r IPRow
		var domains, chains sql.NullString
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &chains); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains.String)
		r.Chains = splitList(chains.String)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RuleIPDomainDetails 对应 GET /api/stats/rules/ips/domain-details。
func (s *Service) RuleIPDomainDetails(backendID int64, rule, ip string, limit int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.SQL().Query(`
		SELECT rd.domain, COALESCE(d.ips,''), rd.total_upload, rd.total_download,
		       rd.total_connections, COALESCE(rd.last_seen,''), COALESCE(d.chains,'')
		FROM rule_domain_traffic rd
		LEFT JOIN domain_stats d
		  ON d.backend_id = rd.backend_id AND d.domain = rd.domain
		WHERE rd.backend_id = ? AND rd.rule = ?
		ORDER BY (rd.total_upload + rd.total_download) DESC, rd.domain
		LIMIT ?`, backendID, rule, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DomainRow{}
	for rows.Next() {
		var d DomainRow
		var ips, chains string
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &chains); err != nil {
			return nil, err
		}
		d.IPs = splitList(ips)
		d.Rules = []string{rule}
		d.Chains = splitList(chains)
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- 设备下钻

// DeviceDomains 对应 GET /api/stats/devices/domains。sourceIP 必填。
func (s *Service) DeviceDomains(backendID int64, sourceIP string, limit int) ([]DomainRow, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.db.SQL().Query(`
		SELECT dd.domain, COALESCE(d.ips,''), dd.total_upload, dd.total_download,
		       dd.total_connections, COALESCE(dd.last_seen,''),
		       COALESCE(d.rules,''), COALESCE(d.chains,'')
		FROM device_domain_stats dd
		LEFT JOIN domain_stats d
		  ON d.backend_id = dd.backend_id AND d.domain = dd.domain
		WHERE dd.backend_id = ? AND dd.source_ip = ?
		ORDER BY (dd.total_upload + dd.total_download) DESC, dd.domain
		LIMIT ?`, backendID, sourceIP, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []DomainRow{}
	for rows.Next() {
		var d DomainRow
		var ips, rules, chains sql.NullString
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &rules, &chains); err != nil {
			return nil, err
		}
		d.IPs = splitList(ips.String)
		d.Rules = splitList(rules.String)
		d.Chains = splitList(chains.String)
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceIPs 对应 GET /api/stats/devices/ips。sourceIP 必填。
func (s *Service) DeviceIPs(backendID int64, sourceIP string, limit int) ([]IPRow, error) {
	if limit <= 0 {
		limit = 5000
	}
	rows, err := s.db.SQL().Query(`
		SELECT di.ip, COALESCE(i.domains,''), di.total_upload, di.total_download,
		       di.total_connections, COALESCE(di.last_seen,''),
		       COALESCE(i.asn,''), COALESCE(i.chains,'')
		FROM device_ip_stats di
		LEFT JOIN ip_stats i
		  ON i.backend_id = di.backend_id AND i.ip = di.ip
		WHERE di.backend_id = ? AND di.source_ip = ?
		ORDER BY (di.total_upload + di.total_download) DESC, di.ip
		LIMIT ?`, backendID, sourceIP, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []IPRow{}
	for rows.Next() {
		var r IPRow
		var domains, chains sql.NullString
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &chains); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains.String)
		r.Chains = splitList(chains.String)
		out = append(out, r)
	}
	return out, rows.Err()
}
