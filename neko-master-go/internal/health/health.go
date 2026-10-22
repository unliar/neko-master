// Package health 实现 backend 健康检查循环。
//
// 上游依据 apps/collector/src/modules/backend/backend.service.ts 的
// runHealthChecks() + database/repositories/health.repository.ts 的
// writeHealthLog()。
//
// 为什么要单独一个包：Go 侧此前只写了 backend_health_logs 的建表、
// 索引与查询（/api/backends/health/history），但没有任何代码往里写，
// 所以接口永远返回空数组、页面看不到数据。本包补上「采集侧」。
//
// 语义对齐：
//   - 只检查 listening = 1 且非 agent 的 backend（agent 走心跳机制）
//   - 探测方式是 GET /version，对齐 handleBackendTest 的口径
//   - 成功 → status=healthy + latency_ms；失败 → status=unhealthy + message
//   - 写入按「UTC 分钟」去重（ON CONFLICT backend_id+minute 时覆盖），
//     因此同一分钟内多次检查只会保留最后一次
//   - 状态变化才打日志，避免每分钟刷屏
package health

import (
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
)

// Checker 周期性探测所有 listening backend 的健康状态并落库。
type Checker struct {
	db       *sql.DB
	log      *slog.Logger
	timeout  time.Duration
	insecure bool

	// status 缓存上一次状态，仅用于「变化时打日志」，不参与判定
	lastStatus map[int64]string
}

// New 创建检查器。interval 建议 60s，与上游的分钟粒度对齐。
func New(db *sql.DB, log *slog.Logger, timeout time.Duration, insecure bool) *Checker {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Checker{
		db:         db,
		log:        log,
		timeout:    timeout,
		insecure:   insecure,
		lastStatus: make(map[int64]string, 16),
	}
}

// backend 是一次检查需要的字段。
type backend struct {
	ID    int64
	Name  string
	URL   string
	Token string
	Type  string
}

// Start 启动检查循环，直到 ctx 取消。立即执行一次，之后按 interval。
func (c *Checker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	c.runOnce(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.runOnce(ctx)
		}
	}
}

// runOnce 执行一轮全量检查。
func (c *Checker) runOnce(ctx context.Context) {
	backends, err := c.listeningBackends(ctx)
	if err != nil {
		c.log.Error("health: list backends", "err", err)
		return
	}
	if len(backends) == 0 {
		return
	}

	// 上游对同一分钟内多次检查做覆盖写，所以整轮共用同一个分钟键。
	minute := time.Now().UTC().Format("2006-01-02T15:04")

	for _, be := range backends {
		if isAgentURL(be.URL) {
			// agent 的存活由心跳决定，不走 HTTP 探测。此处跳过，
			// 与上游 isAgentBackendUrl 分支的处理一致。
			continue
		}
		status, latencyMS, message := c.probe(ctx, be)
		c.persist(be, minute, status, latencyMS, message)

		if prev, ok := c.lastStatus[be.ID]; !ok || prev != status {
			c.log.Info("health check",
				"backend", be.ID, "name", be.Name,
				"status", status, "latency_ms", latencyMS)
			c.lastStatus[be.ID] = status
		}
	}
}

// probe 探测单个 backend，返回 (status, latencyMS, message)。
//
// 探测方式与 /api/backends/{id}/test 一致：GET /version。
// Surge 的 version 端点在 /v1 下，由 SurgeClient 处理。
func (c *Checker) probe(ctx context.Context, be backend) (status string, latencyMS int64, message string) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	start := time.Now()
	if strings.EqualFold(be.Type, "surge") {
		sc := gateway.NewSurgeClient(be.URL, be.Token, c.insecure)
		if _, err := sc.Version(ctx); err != nil {
			return "unhealthy", 0, err.Error()
		}
		return "healthy", time.Since(start).Milliseconds(), ""
	}

	cl := gateway.New(be.URL, be.Token, c.insecure)
	if _, err := cl.Version(ctx); err != nil {
		return "unhealthy", 0, err.Error()
	}
	return "healthy", time.Since(start).Milliseconds(), ""
}

// persist 写入一条健康记录。
//
// 对齐上游 writeHealthLog：ON CONFLICT(backend_id, minute) 时覆盖，
// 保证同分钟只留一条（最后一次检查的结果）。
func (c *Checker) persist(be backend, minute, status string, latencyMS int64, message string) {
	var lat any
	if latencyMS > 0 {
		lat = latencyMS
	}
	var msg any
	if message != "" {
		msg = message
	}
	_, err := c.db.Exec(`
		INSERT INTO backend_health_logs (backend_id, minute, status, latency_ms, server_latency_ms, message)
		VALUES (?, ?, ?, ?, NULL, ?)
		ON CONFLICT(backend_id, minute) DO UPDATE SET
			status = excluded.status,
			latency_ms = excluded.latency_ms,
			server_latency_ms = excluded.server_latency_ms,
			message = excluded.message`,
		be.ID, minute, status, lat, msg)
	if err != nil {
		c.log.Error("health: write log", "backend", be.ID, "err", err)
	}
}

// listeningBackends 返回所有 listening = 1 的 backend。
func (c *Checker) listeningBackends(ctx context.Context) ([]backend, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT id, name, url, COALESCE(token,''), COALESCE(type,'clash')
		FROM backend_configs
		WHERE listening = 1 AND enabled = 1
		ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []backend{}
	for rows.Next() {
		var b backend
		if err := rows.Scan(&b.ID, &b.Name, &b.URL, &b.Token, &b.Type); err != nil {
			continue
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// isAgentURL 判断是否为 agent 模式后端，对齐上游 isAgentBackendUrl。
func isAgentURL(url string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(url)), "agent://")
}

// Prune 清理早于 cutoff 的健康记录，对齐上游按保留天数清理。
func (c *Checker) Prune(cutoff time.Time) error {
	_, err := c.db.Exec(`DELETE FROM backend_health_logs WHERE minute < ?`,
		cutoff.UTC().Format("2006-01-02T15:04"))
	return err
}
