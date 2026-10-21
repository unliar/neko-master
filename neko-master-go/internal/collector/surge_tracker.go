// Surge 增量追踪器：把 /v1/requests/recent 轮询转成 TrafficUpdate。
//
// 与 Clash 的 Tracker 差异（上游 surge.collector.ts）：
//   - 键是 request id，不是连接 id
//   - inBytes/outBytes 是累计值，需做差（in=下载，out=上传）
//   - 首次观测的请求：若已有非零字节且不是 preexisting，直接计一次初始量
//     （Surge 没有 Clash 那样的"连接建立时间"，改用 startDate 做宽限判断）
//   - Surge 可能在同一轮或下一轮继续返回已完成的请求，因此用
//     recentlyCompleted 记录最终值防重复计数
package collector

import (
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// surgeStaleTimeout 与 surgeCompletedTTL 对齐上游：
//   - 请求超过 5 分钟没再出现 → 从 active 清除
//   - 完成记录保留 5 分钟，防止 Surge 在后续轮询里重复返回已完成请求
const (
	surgeStaleTimeout  = 5 * time.Minute
	surgeCompletedTTL  = 5 * time.Minute
	surgePreexistGrace = 5 * time.Second
)

// surgeReqState 记录单个请求的上一轮累计值。
type surgeReqState struct {
	lastUpload   int64
	lastDownload int64
	lastSeen     time.Time
	counted      bool
	initialized  bool
}

// surgeCompleted 记录已完成请求的最终值，防重复计数。
type surgeCompleted struct {
	finalUpload   int64
	finalDownload int64
	completedAt   time.Time
}

// SurgeTracker 是单个 backend 的 Surge 请求追踪状态。
type SurgeTracker struct {
	active    map[string]*surgeReqState
	completed map[string]*surgeCompleted
}

// NewSurgeTracker 创建追踪器。
func NewSurgeTracker() *SurgeTracker {
	return &SurgeTracker{
		active:    make(map[string]*surgeReqState, 512),
		completed: make(map[string]*surgeCompleted, 512),
	}
}

// Diff 处理一轮 requests/recent，返回本轮增量。
func (t *SurgeTracker) Diff(data *gateway.SurgeRequestsData, now time.Time) []model.TrafficUpdate {
	if data == nil || len(data.Requests) == 0 {
		t.cleanup(now)
		return nil
	}

	updates := make([]model.TrafficUpdate, 0, len(data.Requests)/2)
	current := make(map[string]struct{}, len(data.Requests))

	for _, req := range data.Requests {
		if req.ID == "" {
			continue
		}
		current[req.ID] = struct{}{}

		// 注意方向：Surge 的 inBytes 是下载，outBytes 是上传
		curUpload := int64(req.OutBytes)
		curDownload := int64(req.InBytes)
		if curUpload < 0 {
			curUpload = 0
		}
		if curDownload < 0 {
			curDownload = 0
		}

		// 已完成请求去重：无新增字节则整条跳过
		if ci, ok := t.completed[req.ID]; ok {
			if curUpload <= ci.finalUpload && curDownload <= ci.finalDownload {
				continue
			}
			// 竟然又有新字节 → 解除完成标记重新处理
			delete(t.completed, req.ID)
		}

		domain, ip := gateway.ResolveTarget(req)
		chains := gateway.ConvertSurgeChains(req.PolicyName, req.OrigPolicyName, req.Notes)
		chain := chains[0]
		sourceIP := gateway.SurgeSourceIP(req.SourceAddress, req.LocalAddress)
		rule := gateway.RuleName(req)
		reqTime := gateway.RequestTime(req)

		existing, seen := t.active[req.ID]
		if !seen {
			// 首次观测
			preexisting := false
			if req.StartDate != nil && *req.StartDate > 0 {
				preexisting = now.Sub(time.Unix(int64(*req.StartDate), 0)) > surgePreexistGrace
			}
			hasInitial := curUpload > 0 || curDownload > 0

			t.active[req.ID] = &surgeReqState{
				lastUpload:   curUpload,
				lastDownload: curDownload,
				lastSeen:     now,
				counted:      hasInitial || (preexisting && hasInitial),
				initialized:  true,
			}

			// 只计「非 preexisting 且有初始字节」的量
			if hasInitial && !preexisting {
				updates = append(updates, model.TrafficUpdate{
					TimestampMs: reqTime,
					Domain:      domain,
					IP:          ip,
					SourceIP:    sourceIP,
					Chain:       chain,
					FullChain:   joinChain(chains),
					Rule:        rule,
					Upload:      curUpload,
					Download:    curDownload,
					Connections: 1,
				})
			}
			t.markCompletedIfDone(req, curUpload, curDownload, now)
			continue
		}

		var upDelta, downDelta int64
		if curUpload < existing.lastUpload || curDownload < existing.lastDownload {
			// 计数器回退（请求被复用）：整体当增量
			upDelta, downDelta = curUpload, curDownload
			existing.counted = false
		} else {
			upDelta = curUpload - existing.lastUpload
			downDelta = curDownload - existing.lastDownload
		}
		existing.lastUpload = curUpload
		existing.lastDownload = curDownload
		existing.lastSeen = now

		if upDelta == 0 && downDelta == 0 {
			t.markCompletedIfDone(req, curUpload, curDownload, now)
			continue
		}

		var conns int64
		if !existing.counted {
			conns = 1
			existing.counted = true
		}
		updates = append(updates, model.TrafficUpdate{
			TimestampMs: reqTime,
			Domain:      domain,
			IP:          ip,
			SourceIP:    sourceIP,
			Chain:       chain,
			FullChain:   joinChain(chains),
			Rule:        rule,
			Upload:      upDelta,
			Download:    downDelta,
			Connections: conns,
		})

		t.markCompletedIfDone(req, curUpload, curDownload, now)
	}

	t.cleanup(now)
	return updates
}

// markCompletedIfDone 在请求标记完成/断开时记录最终值。
func (t *SurgeTracker) markCompletedIfDone(req gateway.SurgeRequest, up, down int64, now time.Time) {
	if !gateway.IsCompleted(req) && !gateway.IsDisconnected(req) {
		return
	}
	t.completed[req.ID] = &surgeCompleted{
		finalUpload:   up,
		finalDownload: down,
		completedAt:   now,
	}
}

// cleanup 清除过期请求与完成记录。
func (t *SurgeTracker) cleanup(now time.Time) {
	for id, st := range t.active {
		if now.Sub(st.lastSeen) > surgeStaleTimeout {
			delete(t.active, id)
		}
	}
	for id, ci := range t.completed {
		if now.Sub(ci.completedAt) > surgeCompletedTTL {
			delete(t.completed, id)
		}
	}
}

// ActiveCount 返回当前追踪的请求数。
func (t *SurgeTracker) ActiveCount() int { return len(t.active) }

// CompletedCount 返回防重计数表中记录数。
func (t *SurgeTracker) CompletedCount() int { return len(t.completed) }
