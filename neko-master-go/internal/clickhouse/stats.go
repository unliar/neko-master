package clickhouse

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// StorageSizeBytes 返回 traffic_* 与 country_* 表的磁盘占用总和。
//
// 对齐上游 resolveClickHouseStorageStats：把 traffic_minute / traffic_detail /
// traffic_agg / country_minute 四张表的 bytes_on_disk 相加。
// 失败时返回错误，由调用方决定是否降级（本项目的约定是降级到 0）。
func (w *Writer) StorageSizeBytes(ctx context.Context) (int64, error) {
	q := "SELECT sum(bytes_on_disk) FROM system.parts " +
		"WHERE database = {db:String} AND active " +
		"AND table IN ('traffic_minute','traffic_detail','traffic_agg','country_minute')"
	body, err := w.textQuery(ctx, q)
	if err != nil {
		return 0, err
	}
	return parseInt64(strings.TrimSpace(string(body))), nil
}

// TrafficMinuteRows 返回 traffic_minute 的行数估计。
//
// 对齐上游取 trafficRows 的口径。用 system.parts 的 rows 而不是
// SELECT COUNT(*)：SummingMergeTree 上做全量 count 代价高，
// 而 parts 元数据已经是现成的行数估计，精度对「展示用量」足够。
func (w *Writer) TrafficMinuteRows(ctx context.Context) (int64, error) {
	q := "SELECT sum(rows) FROM system.parts " +
		"WHERE database = {db:String} AND active AND table = 'traffic_minute'"
	body, err := w.textQuery(ctx, q)
	if err != nil {
		return 0, err
	}
	return parseInt64(strings.TrimSpace(string(body))), nil
}

// textQuery 执行一次返回单行文本的查询。
//
// 与 query() 的区别：这里要拿回响应体。param_db 用 {db:String} 占位符
// 而不是字符串拼接，避免库名里的特殊字符破坏 SQL。
func (w *Writer) textQuery(ctx context.Context, q string) ([]byte, error) {
	params := "param_database=" + urlEscape(w.cfg.Database)
	url := w.cfg.BaseURL() + "/?" + params + "&query=" + urlEscape(q)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return nil, err
	}
	if w.cfg.User != "" || w.cfg.Password != "" {
		req.SetBasicAuth(w.cfg.User, w.cfg.Password)
	}

	resp, err := w.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, &queryError{status: resp.StatusCode, body: body}
	}
	return body, nil
}

type queryError struct {
	status int
	body   []byte
}

func (e *queryError) Error() string {
	n := len(e.body)
	if n > 160 {
		n = 160
	}
	return "clickhouse query: HTTP " + strconv.Itoa(e.status) + ": " + string(e.body[:n])
}

// parseInt64 解析 ClickHouse 的聚合结果。sum() 在无匹配行时返回
// 空字符串（而不是 0），空值按 0 处理。
func parseInt64(s string) int64 {
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// ClickHouse 可能返回浮点（如 1.84467440737096e+19），退一步用 float 解析
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return 0
		}
		return int64(f)
	}
	return n
}
