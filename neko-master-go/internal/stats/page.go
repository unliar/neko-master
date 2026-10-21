package stats

import (
	"fmt"
	"strings"
)

// PageParams 分页与排序参数。
type PageParams struct {
	Offset    int
	Limit     int
	SortBy    string
	SortOrder string // asc | desc
	Search    string
}

// PageResult 是分页查询结果。
type PageResult struct {
	Data  []any `json:"data"`
	Total int   `json:"total"`
}

// DefaultDomainPage / DefaultIPPage 是上游的默认值。
// 注意：WS 侧 domainsPage 的 limit 上限是 200，与 HTTP 的 5000 不同。
var domainSortKeys = map[string]string{
	"domain":   "domain",
	"upload":   "total_upload",
	"download": "total_download",
	"traffic":  "(total_upload + total_download)",
	"conns":    "total_connections",
	"lastSeen": "last_seen",
}

var ipSortKeys = map[string]string{
	"ip":       "ip",
	"upload":   "total_upload",
	"download": "total_download",
	"traffic":  "(total_upload + total_download)",
	"conns":    "total_connections",
	"lastSeen": "last_seen",
}

// NormDomainPage 归一化域名分页参数。
func NormDomainPage(offset, limit *int, sortBy, sortOrder, search *string) (PageParams, bool) {
	return normPageParams(offset, limit, sortBy, sortOrder, search, domainSortKeys)
}

// NormIPPage 归一化 IP 分页参数。
func NormIPPage(offset, limit *int, sortBy, sortOrder, search *string) (PageParams, bool) {
	return normPageParams(offset, limit, sortBy, sortOrder, search, ipSortKeys)
}

// normPageParams 归一化分页参数。
//
// limit 默认 50，上限 200（WS 侧约束）。
// sortBy 不在白名单 → 返回 ok=false，调用方应忽略整个 page 设置（上游行为）。
func normPageParams(offset, limit *int, sortBy, sortOrder, search *string, keys map[string]string) (PageParams, bool) {
	// SortBy 存的是 DB 列名/表达式，默认按总流量降序。
	// 不能先填 "traffic" 再补映射 —— 那个是前端用的 key，
	// 直接拼进 ORDER BY 会报 no such column: traffic。
	p := PageParams{
		Offset:    0,
		Limit:     50,
		SortBy:    keys["traffic"],
		SortOrder: "desc",
	}

	if offset != nil {
		p.Offset = *offset
		if p.Offset < 0 {
			p.Offset = 0
		}
	}
	if limit != nil {
		p.Limit = *limit
		if p.Limit <= 0 {
			p.Limit = 50
		}
		if p.Limit > 200 {
			p.Limit = 200
		}
	}
	if sortBy != nil {
		col, ok := keys[*sortBy]
		if !ok {
			// 白名单外：整个 page 设置被忽略
			return p, false
		}
		p.SortBy = col
	}
	if sortOrder != nil {
		if *sortOrder != "asc" && *sortOrder != "desc" {
			return p, false
		}
		p.SortOrder = *sortOrder
	}
	if search != nil {
		s := strings.TrimSpace(*search)
		if s != "" {
			p.Search = s
		}
	}
	return p, true
}

// ---------------------------------------------------------------- 分页查询

// DomainsPage 查询域名分页。
func (s *Service) DomainsPage(backendID int64, p PageParams) (*PageResult, error) {
	where := "backend_id = ?"
	args := []any{backendID}
	if p.Search != "" {
		where += " AND domain LIKE ?"
		args = append(args, "%"+p.Search+"%")
	}

	var total int
	if err := s.db.SQL().QueryRow(
		"SELECT COUNT(*) FROM domain_stats WHERE "+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	q := fmt.Sprintf(`
		SELECT domain, COALESCE(ips,''), total_upload, total_download,
		       total_connections, COALESCE(last_seen,''),
		       COALESCE(rules,''), COALESCE(chains,'')
		FROM domain_stats WHERE %s
		ORDER BY %s %s, domain
		LIMIT ? OFFSET ?`, where, p.SortBy, strings.ToUpper(p.SortOrder))

	args = append(args, p.Limit, p.Offset)
	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := []any{}
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
		data = append(data, d)
	}
	return &PageResult{Data: data, Total: total}, rows.Err()
}

// IPsPage 查询 IP 分页。
func (s *Service) IPsPage(backendID int64, p PageParams) (*PageResult, error) {
	where := "backend_id = ?"
	args := []any{backendID}
	if p.Search != "" {
		where += " AND ip LIKE ?"
		args = append(args, "%"+p.Search+"%")
	}

	var total int
	if err := s.db.SQL().QueryRow(
		"SELECT COUNT(*) FROM ip_stats WHERE "+where, args...).Scan(&total); err != nil {
		return nil, err
	}

	q := fmt.Sprintf(`
		SELECT ip, COALESCE(domains,''), total_upload, total_download,
		       total_connections, COALESCE(last_seen,''),
		       COALESCE(asn,''), COALESCE(chains,'')
		FROM ip_stats WHERE %s
		ORDER BY %s %s, ip
		LIMIT ? OFFSET ?`, where, p.SortBy, strings.ToUpper(p.SortOrder))

	args = append(args, p.Limit, p.Offset)
	rows, err := s.db.SQL().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := []any{}
	for rows.Next() {
		var r IPRow
		var domains, chains string
		if err := rows.Scan(&r.IP, &domains, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &r.ASN, &chains); err != nil {
			return nil, err
		}
		r.Domains = splitList(domains)
		r.Chains = splitList(chains)
		data = append(data, r)
	}
	return &PageResult{Data: data, Total: total}, rows.Err()
}
