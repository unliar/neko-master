// Package collector 实现从网关快照到流量增量的转换。
//
// 核心逻辑复刻上游 apps/collector/src/modules/collector：
//   - 每轮拉一次 /connections 全量快照
//   - 与上一轮同 id 的累计值做差得到增量
//   - 首次出现的连接只建基线不计流量（preexisting 宽限）
//   - 计数器回退视为 backend 重启 / id 复用，把当前值整体当增量
package collector

import (
	"strconv"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// preexistingGrace 是首次快照连接被视为 preexisting 的时间窗。
// 上游用 5 秒：连接建立时间距离"首次观测"小于 5 秒的才算新流量。
const preexistingGrace = 5 * time.Second

// connState 记录单条连接的上一轮累计值。
type connState struct {
	lastUpload   int64
	lastDownload int64
	lastSeen     time.Time
	counted      bool // 是否已经计过连接数
}

// Tracker 维护单个 backend 的连接追踪状态。
type Tracker struct {
	active map[string]*connState
}

// NewTracker 创建追踪器。
func NewTracker() *Tracker {
	return &Tracker{active: make(map[string]*connState, 1024)}
}

// ParseStart 解析 Mihomo 的 conn.start（RFC3339）。
func parseStart(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// counterValue 取计数值，兼容 Stash 的 {total: n} 对象形态。
func counterValue(raw any) int64 {
	switch v := raw.(type) {
	case float64:
		if v < 0 {
			return 0
		}
		return int64(v)
	case int64:
		if v < 0 {
			return 0
		}
		return v
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0
		}
		return n
	case map[string]any:
		if t, ok := v["total"]; ok {
			return counterValue(t)
		}
		return 0
	}
	return 0
}

// Diff 处理一次全量快照，返回本轮产生的增量记录。
//
// now 是本轮观测时刻。上游用消息到达时刻作为分桶时刻，
// 因此这里只用 now，不用 conn.start。
func (t *Tracker) Diff(resp *model.ConnectionsResponse, now time.Time) []model.TrafficUpdate {
	if resp == nil {
		return nil
	}

	updates := make([]model.TrafficUpdate, 0, len(resp.Connections)/2)
	currentIds := make(map[string]struct{}, len(resp.Connections))

	for _, conn := range resp.Connections {
		id := conn.ID
		currentIds[id] = struct{}{}

		md := conn.Metadata
		domain := md.Host
		if domain == "" {
			// sniffHost 只在部分内核返回，这里退化为空
			domain = ""
		}
		ip := md.DestinationIP
		sourceIP := md.SourceIP

		chain := "DIRECT"
		fullChain := "DIRECT"
		if len(conn.Chains) > 0 {
			chain = conn.Chains[0]
			fullChain = joinChain(conn.Chains)
		}
		rule := conn.Rule
		if rule == "" {
			rule = "Match"
		}

		up, down := conn.Upload, conn.Download

		existing, seen := t.active[id]
		if !seen {
			// 首次观测：判断是否 preexisting
			t.active[id] = &connState{
				lastUpload:   up,
				lastDownload: down,
				lastSeen:     now,
				counted:      false,
			}
			// 连接建立时间在宽限期之外 → preexisting，只建基线不计流量
			if start, ok := parseStart(conn.Start); ok && now.Sub(start) > preexistingGrace {
				continue
			}
			// 新连接且无历史累计 → 无增量可计，等下一轮做差
			continue
		}

		var upDelta, downDelta int64
		if up < existing.lastUpload || down < existing.lastDownload {
			// 计数器回退：整体当增量
			upDelta, downDelta = up, down
			existing.counted = false
		} else {
			upDelta = up - existing.lastUpload
			downDelta = down - existing.lastDownload
		}
		existing.lastUpload = up
		existing.lastDownload = down
		existing.lastSeen = now

		if upDelta == 0 && downDelta == 0 {
			continue
		}

		var conns int64
		if !existing.counted {
			conns = 1
			existing.counted = true
		}

		updates = append(updates, model.TrafficUpdate{
			TimestampMs: now,
			Domain:      domain,
			IP:          ip,
			SourceIP:    sourceIP,
			Chain:       chain,
			FullChain:   fullChain,
			Rule:        rule,
			RulePayload: conn.RulePayload,
			Upload:      upDelta,
			Download:    downDelta,
			Connections: conns,
		})
	}

	// 清理本轮已消失的连接
	for id := range t.active {
		if _, ok := currentIds[id]; !ok {
			delete(t.active, id)
		}
	}

	return updates
}

// joinChain 把链路数组拼成 "a > b > c"。
func joinChain(chains []string) string {
	if len(chains) == 0 {
		return "DIRECT"
	}
	n := 0
	for _, c := range chains {
		n += len(c)
	}
	n += 3 * (len(chains) - 1)

	b := make([]byte, 0, n)
	for i, c := range chains {
		if i > 0 {
			b = append(b, ' ', '>', ' ')
		}
		b = append(b, c...)
	}
	return string(b)
}

// ActiveCount 返回当前追踪中的连接数。
func (t *Tracker) ActiveCount() int { return len(t.active) }
