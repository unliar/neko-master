// Package clickhouse 实现 ClickHouse 旁路双写。
//
// 上游见 apps/collector/src/modules/clickhouse/clickhouse.config.ts。
// 语义对齐：
//   - CH_ENABLED != 1 时整个模块禁用，任何调用都是 no-op
//   - CH_REQUIRED=1 时连接失败直接让启动失败；否则告警后降级为禁用
//   - CH_AUTO_CREATE_TABLES=0 时跳过建表
//   - 写入统一走 *_buffer 表，以读到尚未落盘的近期数据
//   - 写入失败计入 unhealthy 连续次数，超过 CH_UNHEALTHY_THRESHOLD 后自动关闭
//
// 本模块只做写入（旁路），不做读取。SQLite 仍是唯一权威数据源，
// 因此任何 ClickHouse 故障都不会影响主流程。
package clickhouse

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// Config 对齐上游 loadClickHouseConfig()。
type Config struct {
	Enabled     bool
	Required    bool
	AutoCreate  bool
	Host        string
	Port        int
	Database    string
	User        string
	Password    string
	Timeout     time.Duration
	MaxRetries  int
	RetryDelay  time.Duration
	MaxPending  int
	UnhealthyAt int
}

// LoadConfig 从环境变量读取配置。
func LoadConfig(getenv func(string, string) string) *Config {
	return &Config{
		Enabled:     getenv("CH_ENABLED", "") == "1",
		Required:    getenv("CH_REQUIRED", "") == "1",
		AutoCreate:  getenv("CH_AUTO_CREATE_TABLES", "1") != "0",
		Host:        getenv("CH_HOST", "clickhouse"),
		Port:        envInt(getenv("CH_PORT", ""), 8123),
		Database:    getenv("CH_DATABASE", "neko_master"),
		User:        getenv("CH_USER", "default"),
		Password:    getenv("CH_PASSWORD", ""),
		Timeout:     time.Duration(envInt(getenv("CH_CONNECT_TIMEOUT_MS", ""), 5000)) * time.Millisecond,
		MaxRetries:  envInt(getenv("CH_CONNECT_MAX_RETRIES", ""), 5),
		RetryDelay:  time.Duration(envInt(getenv("CH_CONNECT_RETRY_DELAY_MS", ""), 2000)) * time.Millisecond,
		MaxPending:  envInt(getenv("CH_WRITE_MAX_PENDING_ROWS", ""), 200000),
		UnhealthyAt: envInt(getenv("CH_UNHEALTHY_THRESHOLD", ""), 5),
	}
}

func envInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// BaseURL 返回 HTTP 端点。
func (c *Config) BaseURL() string {
	scheme := "http"
	return fmt.Sprintf("%s://%s:%d", scheme, c.Host, c.Port)
}

// Writer 是 ClickHouse 旁路写入器。
//
// 串行写入（enqueue 链）避免同一条 batch 乱序到达。
type Writer struct {
	cfg *Config
	hc  *http.Client

	mu       sync.Mutex
	queue    []job // 待写入任务
	running  bool
	pendingN int

	// 健康状态：连续失败计数，超过阈值自动关闭
	fails int
	dead  bool

	// 统计（原子）
	Writes   int64
	Rows     int64
	Failures int64
	Dropped  int64
}

type job struct {
	table string
	rows  [][]any
	note  string
}

// NewWriter 构造写入器。cfg.Enabled=false 时返回 nil（调用方判空即可）。
func NewWriter(cfg *Config) *Writer {
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	return &Writer{
		cfg: cfg,
		hc:  &http.Client{Timeout: cfg.Timeout},
	}
}

// Enabled 报告模块当前是否可用。
func (w *Writer) Enabled() bool {
	if w == nil {
		return false
	}
	return !w.dead
}

// EnsureSchema 建表。上游建 3 张主表 + 3 张 buffer 表。
func (w *Writer) EnsureSchema() error {
	if w == nil || !w.cfg.AutoCreate {
		return nil
	}

	db := w.cfg.Database
	stmts := []string{
		"CREATE DATABASE IF NOT EXISTS " + db,

		"CREATE TABLE IF NOT EXISTS " + db + ".traffic_minute (" +
			"backend_id UInt32, minute DateTime, domain String, ip String, " +
			"source_ip String, chain String, rule String, " +
			"upload UInt64, download UInt64, connections UInt32) " +
			"ENGINE = SummingMergeTree " +
			"PARTITION BY toYYYYMM(minute) " +
			"ORDER BY (backend_id, minute, domain, ip, source_ip, chain, rule) " +
			"TTL minute + INTERVAL 90 DAY " +
			"SETTINGS index_granularity = 8192",

		"CREATE TABLE IF NOT EXISTS " + db + ".country_minute (" +
			"backend_id UInt32, minute DateTime, country String, country_name String, " +
			"continent String, upload UInt64, download UInt64, connections UInt32) " +
			"ENGINE = SummingMergeTree " +
			"PARTITION BY toYYYYMM(minute) " +
			"ORDER BY (backend_id, minute, country) " +
			"TTL minute + INTERVAL 90 DAY " +
			"SETTINGS index_granularity = 8192",

		"CREATE TABLE IF NOT EXISTS " + db + ".traffic_agg (" +
			"backend_id UInt32, minute DateTime, " +
			"upload UInt64, download UInt64, connections UInt32) " +
			"ENGINE = SummingMergeTree " +
			"PARTITION BY toYYYYMM(minute) " +
			"ORDER BY (backend_id, minute) " +
			"TTL minute + INTERVAL 90 DAY " +
			"SETTINGS index_granularity = 8192",

		"CREATE TABLE IF NOT EXISTS " + db + ".traffic_detail AS " + db + ".traffic_minute " +
			"ENGINE = MergeTree " +
			"PARTITION BY toYYYYMM(minute) " +
			"ORDER BY (backend_id, minute, domain, ip) " +
			"TTL minute + INTERVAL 90 DAY " +
			"SETTINGS index_granularity = 8192",

		// 三张 buffer 表，参数与上游一致
		"CREATE TABLE IF NOT EXISTS " + db + ".traffic_agg_buffer AS " + db + ".traffic_agg " +
			"ENGINE = Buffer('" + db + "', 'traffic_agg', 4, 10, 60, 100, 10000, 10000, 1000000)",

		"CREATE TABLE IF NOT EXISTS " + db + ".traffic_detail_buffer AS " + db + ".traffic_detail " +
			"ENGINE = Buffer('" + db + "', 'traffic_detail', 4, 10, 60, 100, 10000, 10000, 1000000)",

		"CREATE TABLE IF NOT EXISTS " + db + ".country_buffer AS " + db + ".country_minute " +
			"ENGINE = Buffer('" + db + "', 'country_minute', 4, 10, 60, 100, 10000, 10000, 1000000)",
	}

	for _, q := range stmts {
		if err := w.query(q); err != nil {
			return err
		}
	}
	return nil
}

// WriteTraffic 双写一批流量增量。
//
// 对齐上游 writeTrafficBatch：detail 行写 traffic_detail_buffer，
// 并按 (backend_id, minute) 预聚合出一行写 traffic_agg_buffer。
func (w *Writer) WriteTraffic(backendID int64, updates []model.TrafficUpdate) error {
	if w == nil || len(updates) == 0 || !w.Enabled() {
		return nil
	}

	detail := make([][]any, 0, len(updates))
	aggIdx := make(map[string]int, len(updates))
	var agg [][]any

	for _, u := range updates {
		minute := u.TimestampMs.UTC().Truncate(time.Minute)
		detail = append(detail, []any{
			uint32(backendID),
			minute.Format("2006-01-02 15:04:05"),
			u.Domain, u.IP, u.SourceIP, u.FullChain, u.Rule,
			clampU64(u.Upload), clampU64(u.Download), clampU32(u.Connections),
		})
		k := minute.Format("2006-01-02 15:04:05")
		if i, ok := aggIdx[k]; ok {
			agg[i][2] = agg[i][2].(uint64) + clampU64(u.Upload)
			agg[i][3] = agg[i][3].(uint64) + clampU64(u.Download)
			agg[i][4] = agg[i][4].(uint32) + clampU32(u.Connections)
		} else {
			aggIdx[k] = len(agg)
			agg = append(agg, []any{
				uint32(backendID), k, clampU64(u.Upload), clampU64(u.Download), clampU32(u.Connections),
			})
		}
	}

	if err := w.enqueue("traffic_detail_buffer", detail, "traffic"); err != nil {
		return err
	}
	return w.enqueue("traffic_agg_buffer", agg, "traffic-agg")
}

// WriteCountries 双写一批国家维度增量。
func (w *Writer) WriteCountries(backendID int64, rows []CountryRow) error {
	if w == nil || len(rows) == 0 || !w.Enabled() {
		return nil
	}
	out := make([][]any, 0, len(rows))
	for _, r := range rows {
		out = append(out, []any{
			uint32(backendID),
			r.TimestampMs.UTC().Truncate(time.Minute).Format("2006-01-02 15:04:05"),
			r.Country, r.CountryName, r.Continent,
			clampU64(r.Upload), clampU64(r.Download), clampU32(r.Connections),
		})
	}
	return w.enqueue("country_buffer", out, "country")
}

// CountryRow 是国家维度的一行。
type CountryRow struct {
	TimestampMs time.Time
	Country     string
	CountryName string
	Continent   string
	Upload      int64
	Download    int64
	Connections int64
}

// enqueue 把一次写入挂到串行链上。队列超限时丢弃并计数。
func (w *Writer) enqueue(table string, rows [][]any, note string) error {
	w.mu.Lock()
	if w.pendingN+len(rows) > w.cfg.MaxPending {
		w.mu.Unlock()
		atomic.AddInt64(&w.Dropped, int64(len(rows)))
		atomic.AddInt64(&w.Failures, 1)
		return fmt.Errorf("clickhouse write queue full (%d rows dropped)", len(rows))
	}
	w.queue = append(w.queue, job{table: table, rows: rows, note: note})
	w.pendingN += len(rows)
	start := !w.running
	w.running = true
	w.mu.Unlock()

	if start {
		go w.drain()
	}
	return nil
}

// drain 串行消费队列。链本身永远恢复，保证后续写入能继续。
func (w *Writer) drain() {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		j := w.queue[0]
		w.queue = w.queue[1:]
		w.pendingN -= len(j.rows)
		w.mu.Unlock()

		if err := w.insert(j.table, j.rows); err != nil {
			w.onFailure(err, j)
			continue
		}
		w.mu.Lock()
		w.fails = 0
		w.mu.Unlock()
		atomic.AddInt64(&w.Writes, 1)
		atomic.AddInt64(&w.Rows, int64(len(j.rows)))
	}
}

// onFailure 处理一次写入失败：计数、超阈值自动关闭。
func (w *Writer) onFailure(err error, j job) {
	atomic.AddInt64(&w.Failures, 1)

	w.mu.Lock()
	w.fails++
	dead := w.fails >= w.cfg.UnhealthyAt
	if dead {
		w.dead = true
	}
	fails := w.fails
	w.mu.Unlock()

	if dead {
		// 自动关闭后不再告警刷屏；调用方可通过 Enabled() 观察
		return
	}
	_ = err
	_ = fails
}

// insert 以 JSONEachRow 写入指定表。
func (w *Writer) insert(table string, rows [][]any) error {
	if len(rows) == 0 {
		return nil
	}
	body, err := encodeJSONEachRow(table, rows)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/?database=%s&query=%s",
		w.cfg.BaseURL(),
		urlEscape(w.cfg.Database),
		urlEscape("INSERT INTO "+w.cfg.Database+"."+table+" FORMAT JSONEachRow"))

	req, err := http.NewRequest(http.MethodPost, url, stringsReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.cfg.User != "" || w.cfg.Password != "" {
		req.SetBasicAuth(w.cfg.User, w.cfg.Password)
	}

	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf := make([]byte, 160)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("clickhouse insert %s: HTTP %d: %s", table, resp.StatusCode, string(buf[:n]))
	}
	return nil
}

// query 执行一次建表语句。
func (w *Writer) query(q string) error {
	url := fmt.Sprintf("%s/?query=%s", w.cfg.BaseURL(), urlEscape(q))
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	if w.cfg.User != "" || w.cfg.Password != "" {
		req.SetBasicAuth(w.cfg.User, w.cfg.Password)
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		buf := make([]byte, 160)
		n, _ := resp.Body.Read(buf)
		return fmt.Errorf("clickhouse query: HTTP %d: %s", resp.StatusCode, string(buf[:n]))
	}
	return nil
}

// Healthy 返回健康状态与统计快照。
func (w *Writer) Healthy() map[string]any {
	if w == nil {
		return map[string]any{"enabled": false}
	}
	w.mu.Lock()
	dead, fails := w.dead, w.fails
	w.mu.Unlock()
	return map[string]any{
		"enabled":  !dead,
		"endpoint": w.cfg.BaseURL(),
		"database": w.cfg.Database,
		"user":     w.cfg.User,
		"fails":    fails,
		"writes":   atomic.LoadInt64(&w.Writes),
		"rows":     atomic.LoadInt64(&w.Rows),
		"failures": atomic.LoadInt64(&w.Failures),
		"dropped":  atomic.LoadInt64(&w.Dropped),
	}
}

// Close 等待队列排空（有界等待）。
func (w *Writer) Close() {
	if w == nil {
		return
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		empty := len(w.queue) == 0
		w.mu.Unlock()
		if empty {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func clampU64(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

func clampU32(v int64) uint32 {
	if v < 0 {
		return 0
	}
	if v > 4294967295 {
		return 4294967295
	}
	return uint32(v)
}
