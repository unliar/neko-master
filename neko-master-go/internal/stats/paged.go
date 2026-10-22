package stats

import (
	"strings"
	"time"
)

// PageOptions 是分页查询参数，对齐上游 PaginatedStatsOptions。
type PageOptions struct {
	Offset    int
	Limit     int
	SortBy    string
	SortOrder string
	Search    string
}

// PagedResult 对齐上游 PaginatedDomainStats / PaginatedIPStats：
// 必须是 {data, total} 结构。此前直接返回裸数组，
// 前端读 res.data 得到 undefined，页面一片空白。
type PagedResult[T any] struct {
	Data  []T   `json:"data"`
	Total int64 `json:"total"`
}

// sortColumn 把前端的 sortBy 映射为真实列名。
//
// 前端传的是 totalTraffic / totalUpload / totalDownload /
// totalConnections 之类的逻辑名，不能直接拼进 SQL。
// 白名单映射而非字符串拼接，杜绝注入。
var domainSortCols = map[string]string{
	"domain":           "domain",
	"totalupload":      "total_upload",
	"totaldownload":    "total_download",
	"totalconnections": "total_connections",
	"totaltraffic":     "(total_upload + total_download)",
	"traffic":          "(total_upload + total_download)",
	"lastseen":         "last_seen",
}

var ipSortCols = map[string]string{
	"ip":               "ip",
	"totalupload":      "total_upload",
	"totaldownload":    "total_download",
	"totalconnections": "total_connections",
	"totaltraffic":     "(total_upload + total_download)",
	"traffic":          "(total_upload + total_download)",
	"lastseen":         "last_seen",
}

// resolveSort 归一化 sortBy / sortOrder，返回列表达式与方向。
// 无法识别时回落默认列，方向只认 desc（其余一律 asc）。
func resolveSort(cols map[string]string, defCol, sortBy, sortOrder string) (col, dir string) {
	key := strings.ToLower(strings.TrimSpace(sortBy))
	col, ok := cols[key]
	if !ok {
		col = defCol
	}
	dir = "ASC"
	if strings.EqualFold(strings.TrimSpace(sortOrder), "desc") {
		dir = "DESC"
	}
	return col, dir
}

// DomainsPaged 返回分页域名统计。
//
// start / end 为 nil 时不加时间过滤（此时等价于旧的 TopDomains）。
// 上游 getDomainStatsPaginated 在传了时间范围时按 last_seen 过滤，
// 这里保持一致：domain_stats 只有 last_seen 这一列时间信息。
func (s *Service) DomainsPaged(backendID int64, o PageOptions, start, end *time.Time) (PagedResult[DomainRow], error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	col, dir := resolveSort(domainSortCols, "(total_upload + total_download)", o.SortBy, o.SortOrder)

	where := []string{"backend_id = ?"}
	args := []any{backendID}
	if o.Search != "" {
		where = append(where, "domain LIKE ?")
		args = append(args, "%"+o.Search+"%")
	}
	if start != nil {
		where = append(where, "last_seen >= ?")
		args = append(args, start.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if end != nil {
		where = append(where, "last_seen <= ?")
		args = append(args, end.UTC().Format("2006-01-02T15:04:05Z"))
	}
	clause := "WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := s.db.SQL().QueryRow(
		"SELECT COUNT(*) FROM domain_stats "+clause, args...).Scan(&total); err != nil {
		return PagedResult[DomainRow]{}, err
	}

	q := `SELECT domain, COALESCE(ips,''), total_upload, total_download, total_connections,
	             COALESCE(last_seen,''), COALESCE(rules,''), COALESCE(chains,'')
	      FROM domain_stats ` + clause +
		` ORDER BY ` + col + ` ` + dir + `, domain LIMIT ? OFFSET ?`

	qargs := append(append([]any{}, args...), o.Limit, o.Offset)
	rows, err := s.db.SQL().Query(q, qargs...)
	if err != nil {
		return PagedResult[DomainRow]{}, err
	}
	defer rows.Close()

	out := make([]DomainRow, 0, o.Limit)
	for rows.Next() {
		var d DomainRow
		var ips, rules, chains string
		if err := rows.Scan(&d.Domain, &ips, &d.TotalUpload, &d.TotalDownload,
			&d.TotalConnections, &d.LastSeen, &rules, &chains); err != nil {
			continue
		}
		d.IPs = splitList(ips)
		d.Rules = splitList(rules)
		d.Chains = splitList(chains)
		out = append(out, d)
	}
	return PagedResult[DomainRow]{Data: out, Total: total}, rows.Err()
}

// IPsPaged 返回分页 IP 统计，结构与 DomainsPaged 一致。
func (s *Service) IPsPaged(backendID int64, o PageOptions, start, end *time.Time) (PagedResult[IPRow], error) {
	if o.Limit <= 0 {
		o.Limit = 50
	}
	col, dir := resolveSort(ipSortCols, "(total_upload + total_download)", o.SortBy, o.SortOrder)

	where := []string{"backend_id = ?"}
	args := []any{backendID}
	if o.Search != "" {
		where = append(where, "ip LIKE ?")
		args = append(args, "%"+o.Search+"%")
	}
	if start != nil {
		where = append(where, "last_seen >= ?")
		args = append(args, start.UTC().Format("2006-01-02T15:04:05Z"))
	}
	if end != nil {
		where = append(where, "last_seen <= ?")
		args = append(args, end.UTC().Format("2006-01-02T15:04:05Z"))
	}
	clause := "WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := s.db.SQL().QueryRow(
		"SELECT COUNT(*) FROM ip_stats "+clause, args...).Scan(&total); err != nil {
		return PagedResult[IPRow]{}, err
	}

	// IPRow 没有 rules 字段（对齐前端 IPStats），这里取 asn 与 chains。
	q := `SELECT ip, COALESCE(domains,''), total_upload, total_download, total_connections,
	             COALESCE(last_seen,''), COALESCE(asn,''), COALESCE(chains,'')
	      FROM ip_stats ` + clause +
		` ORDER BY ` + col + ` ` + dir + `, ip LIMIT ? OFFSET ?`

	qargs := append(append([]any{}, args...), o.Limit, o.Offset)
	rows, err := s.db.SQL().Query(q, qargs...)
	if err != nil {
		return PagedResult[IPRow]{}, err
	}
	defer rows.Close()

	out := make([]IPRow, 0, o.Limit)
	for rows.Next() {
		var r IPRow
		var doms, asn, chains string
		if err := rows.Scan(&r.IP, &doms, &r.TotalUpload, &r.TotalDownload,
			&r.TotalConnections, &r.LastSeen, &asn, &chains); err != nil {
			continue
		}
		r.Domains = splitList(doms)
		r.ASN = asn
		r.Chains = splitList(chains)
		out = append(out, r)
	}
	return PagedResult[IPRow]{Data: out, Total: total}, rows.Err()
}
