// Package realtime 实现「实时叠加」：把尚未落盘的增量叠加到 DTO 上。
//
// 问题：采集器每轮产生增量先进 Buffer，每 30s（FLUSH_INTERVAL_MS）才落库，
// 因此 stats.Summary() 这类直接查 SQLite 的 DTO 会落后最多一个 flush 周期，
// WS 推送跟着掉。
//
// 本实现逐函数对齐上游 apps/collector/src/modules/realtime/realtime.store.ts：
//
//	recordTraffic()   -> Store.Record
//	getSummaryDelta() -> Store.SummaryDelta
//	getTodayDelta()   -> Store.TodayDelta
//	clearTrafficSummary()  -> Store.ClearTrafficSummary
//	clearTrafficDimensions() -> Store.ClearTrafficDimensions
//	clearTraffic()    -> Store.ClearTraffic
//	pruneOldBuckets() -> Store.pruneOldBuckets
//
// 与上游一致的两条关键语义：
//
//  1. summary 是一个**永不自动清理的累加器**（SummaryDelta），
//     只在 flush 成功后由调用方 ClearTraffic* 清零。这意味着 flush 失败时
//     缓冲保留，下次 flush 会带着上次的数据重试 —— 数据不会丢。
//  2. recordTraffic 有早退：upload <= 0 && download <= 0 时直接返回，
//     连 minute bucket 都不建。上游用 `connections = 1` 做默认值单独累加，
//     因此连接数只在该条增量真正入账时才 +1。
package realtime

import (
	"strconv"
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// SummaryDelta 对齐上游 SummaryDelta。
//
// 注意：这是累加器，不是快照。ClearTrafficSummary 之前它只增不减。
type SummaryDelta struct {
	Upload      int64
	Download    int64
	Connections int64
	LastUpdated time.Time
}

// MinuteBucket 对齐上游 MinuteBucket（只留 upload/download，
// lastUpdated 用于调试，上游也保留）。
type MinuteBucket struct {
	Upload      int64
	Download    int64
	LastUpdated time.Time
}

// Store 是单 backend 的实时叠加缓冲。
//
// 上游用 8 个 Map 分别承载 summary / minute / domain / ip / proxy / device /
// deviceDomain / deviceIP / rule / ruleChain / country。Go 侧目前只实现
// Summary 实际用到的 summary 与 minute 两个；维度 Map 尚未被 Summary 读取，
// 属于上游已备但当前未接线的部分，见 README「已知差异」。
type Store struct {
	mu      sync.Mutex
	summary SummaryDelta
	minute  map[string]*MinuteBucket
	maxMin  int
}

// NewStore 创建缓冲。maxMinutes 对齐上游 maxMinutes（REALTIME_MAX_MINUTES）。
func NewStore(maxMinutes int) *Store {
	if maxMinutes <= 0 {
		maxMinutes = 180
	}
	return &Store{
		minute: make(map[string]*MinuteBucket, 256),
		maxMin: maxMinutes,
	}
}

// Record 累加一条增量，对齐 recordTraffic()。
//
// 早退语义必须与上游一致：upload 与 download 都 <= 0 时直接返回，
// 不建 minute bucket、不累加连接数。
func (s *Store) Record(u model.TrafficUpdate) {
	if u.Upload <= 0 && u.Download <= 0 {
		return
	}

	ts := u.TimestampMs
	if ts.IsZero() {
		ts = time.Now()
	}

	conns := u.Connections
	if conns <= 0 {
		// 上游 recordTraffic 的 connections 形参默认值是 1；
		// 采集器在首次计量时显式传 1，其余时候传 0。
		// 这里保持「调用方给多少就加多少」，0 就加 0。
		conns = 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.summary.Upload += u.Upload
	s.summary.Download += u.Download
	s.summary.Connections += conns
	s.summary.LastUpdated = ts

	key := minuteKey(ts)
	b, ok := s.minute[key]
	if !ok {
		b = &MinuteBucket{}
		s.minute[key] = b
	}
	b.Upload += u.Upload
	b.Download += u.Download
	b.LastUpdated = ts

	s.pruneOldBucketsLocked(ts)
}

// SummaryDelta 返回当前累加器，对齐 getSummaryDelta()。
func (s *Store) SummaryDelta() SummaryDelta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.summary
}

// TodayDelta 返回今日（UTC）未落盘增量，对齐 getTodayDelta()。
//
// 上游按 toISOString().slice(0,10) 前缀匹配分钟键，即按 UTC 日期切分。
func (s *Store) TodayDelta(now time.Time) (upload, download int64) {
	todayPrefix := now.UTC().Format("2006-01-02")

	s.mu.Lock()
	defer s.mu.Unlock()

	for k, b := range s.minute {
		if len(k) >= 10 && k[:10] == todayPrefix {
			upload += b.Upload
			download += b.Download
		}
	}
	return upload, download
}

// ClearTrafficSummary 清空 summary 与 minute，对齐 clearTrafficSummary()。
//
// 只在 flush 成功后调用。flush 失败时**不要**调用，否则那批数据永久丢失。
func (s *Store) ClearTrafficSummary() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary = SummaryDelta{}
	s.minute = make(map[string]*MinuteBucket, 256)
}

// ClearTrafficDimensions 清空维度侧缓存。
//
// 上游对应 domain / ip / proxy / device / deviceDomain / deviceIP /
// rule / ruleChain 八个 Map。Go 侧这些 Map 尚未实现（Summary 不用），
// 故当前是 no-op，保留函数以对齐调用点与后续扩展。
func (s *Store) ClearTrafficDimensions() {
	// no-op：维度 Map 未实现，见 Store 注释中的「已知差异」。
}

// ClearTraffic 同时清空 summary 与维度，对齐 clearTraffic()。
func (s *Store) ClearTraffic() {
	s.ClearTrafficSummary()
	s.ClearTrafficDimensions()
}

// Clear 是 ClearTraffic 的别名，便于调用方按语境选用。
func (s *Store) Clear() { s.ClearTraffic() }

// Len 返回当前 minute bucket 数（调试用）。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.minute)
}

// minuteKey 对齐上游 toMinuteKey：ISO 前 16 位 + ":00"，即 UTC 分钟桶。
func minuteKey(ts time.Time) string {
	return ts.UTC().Format("2006-01-02T15:04") + ":00"
}

// pruneOldBuckets 对齐上游 pruneOldBuckets。
//
// 上游按「nowMs - maxMinutes*60_000」取 cutoffKey，
// 删除 key < cutoffKey 的桶。这是相对时间的滚动窗口，
// 与「flush 时刻裁剪」是两件不同的事 —— 不要混用。
//
// 注意：该函数只裁剪 minute 桶，**不清 summary 累加器**。
// 累加器只能靠 ClearTrafficSummary 清零。
func (s *Store) pruneOldBucketsLocked(now time.Time) {
	cutoff := minuteKey(now.Add(-time.Duration(s.maxMin) * time.Minute))
	for k := range s.minute {
		if k < cutoff {
			delete(s.minute, k)
		}
	}
}

// ---------------------------------------------------------------- Manager

// Manager 按 backend 管理多个 Store，对齐上游单例的按 backend 分片。
type Manager struct {
	mu     sync.Mutex
	stores map[int64]*Store
	maxMin int
}

// NewManager 创建管理器。
func NewManager(maxMinutes int) *Manager {
	return &Manager{stores: make(map[int64]*Store), maxMin: maxMinutes}
}

// Store 返回指定 backend 的缓冲（不存在则创建）。
func (m *Manager) Store(backendID int64) *Store {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.stores[backendID]
	if !ok {
		st = NewStore(m.maxMin)
		m.stores[backendID] = st
	}
	return st
}

// Record 记录一条增量到对应 backend 的缓冲。
func (m *Manager) Record(backendID int64, u model.TrafficUpdate) {
	m.Store(backendID).Record(u)
}

// Drop 移除某个 backend 的缓冲（backend 被删除时调用）。
func (m *Manager) Drop(backendID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stores, backendID)
}

// ClearTraffic 对指定 backend 执行 clearTraffic。
func (m *Manager) ClearTraffic(backendID int64) { m.Store(backendID).ClearTraffic() }

// MinuteKeyForTest 暴露 minuteKey 给测试，避免测试包重复实现同一格式。
func MinuteKeyForTest(ts time.Time) string { return minuteKey(ts) }

// FormatInt 供测试构造期望值。
func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }
