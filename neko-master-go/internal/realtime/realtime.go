// Package realtime 实现「实时叠加」：把尚未落盘的增量叠加到 DTO 上。
//
// 问题：采集器每轮产生增量先进 Buffer，每 30s（FLUSH_INTERVAL_MS）才落库。
// 因此 stats.Summary() 这类直接查 SQLite 的 DTO 会落后最多一个 flush 周期。
// WS 推送跟着掉，用户看到的是半分钟前的数字。
//
// 解法：采集器每产生一条增量就 recordTraffic() 到内存 buffer；
// 构建 DTO 时用 overlay() 把 buffer 里的量加回去。
// 上游对应 apps/collector/src/modules/realtime/realtime-store.ts。
//
// 一致性保证：overlay 只加不重算。同一个 (domain, ip, chain, rule) 键的
// 增量在 SQLite 与 buffer 中只会各占一份，落盘后 buffer 清零，
// 因此不会重复计数。近似性在于：唯一维度计数（TotalDomains/TotalIPs）
// 无法从增量推出新增了几个唯一键，故这几项只在 buffer 中已有的键
// 明显可能新增时才上调，宁可少报也不错报。
package realtime

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// Delta 是一份未落盘增量。
type Delta struct {
	Domain      string
	IP          string
	SourceIP    string
	Chain       string
	FullChain   string
	Rule        string
	Upload      int64
	Download    int64
	Connections int64
	TimestampMs time.Time
}

// Store 是单 backend 的实时叠加缓冲。
type Store struct {
	mu       sync.Mutex
	perMin   map[string]*Delta // key = minute + 维度
	total    Delta             // 全量累计（用于 total 字段）
	lastSeen time.Time
	maxMin   int
}

// NewStore 创建缓冲。maxMinute 限制保留的分钟桶数（对齐 REALTIME_MAX_MINUTES）。
func NewStore(maxMinute int) *Store {
	if maxMinute <= 0 {
		maxMinute = 180
	}
	return &Store{
		perMin: make(map[string]*Delta, 256),
		maxMin: maxMinute,
	}
}

// Record 累加一条增量。
func (s *Store) Record(u model.TrafficUpdate) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := u.TimestampMs
	if now.IsZero() {
		now = time.Now()
	}
	s.lastSeen = now

	minute := now.UTC().Truncate(time.Minute).Format("2006-01-02T15:04")
	key := minute + "|" + u.Domain + "|" + u.IP + "|" + u.SourceIP + "|" + u.FullChain + "|" + u.Rule

	d, ok := s.perMin[key]
	if !ok {
		d = &Delta{
			Domain: u.Domain, IP: u.IP, SourceIP: u.SourceIP,
			Chain: u.Chain, FullChain: u.FullChain, Rule: u.Rule,
			TimestampMs: now,
		}
		s.perMin[key] = d
	}
	d.Upload += u.Upload
	d.Download += u.Download
	d.Connections += u.Connections
	if now.After(d.TimestampMs) {
		d.TimestampMs = now
	}

	s.total.Upload += u.Upload
	s.total.Download += u.Download
	s.total.Connections += u.Connections
}

// Snapshot 返回当前缓冲的一份拷贝。
func (s *Store) Snapshot() Delta {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

// Since 返回指定时刻（含）之后的增量汇总。
//
// 用于「今日流量」这类带时间下界的字段：只叠加真正落在区间内的部分。
func (s *Store) Since(t time.Time) Delta {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out Delta
	for _, d := range s.perMin {
		if d.TimestampMs.Before(t) {
			continue
		}
		out.Upload += d.Upload
		out.Download += d.Download
		out.Connections += d.Connections
	}
	return out
}

// Deltas 返回缓冲中所有增量（按分钟升序），供 Top 排行叠加使用。
func (s *Store) Deltas() []Delta {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Delta, 0, len(s.perMin))
	for _, d := range s.perMin {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimestampMs.Before(out[j].TimestampMs) })
	return out
}

// Prune 丢弃早于 cutoff 的分钟桶。
//
// 调用方应在每次 Flush 后执行：既然 SQLite 已经有了这些数据，
// buffer 里对应的部分就该丢掉，否则会和数据库重复计数。
func (s *Store) Prune(cutoff time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cut := cutoff.UTC().Truncate(time.Minute).Format("2006-01-02T15:04")
	for k := range s.perMin {
		// key 的前 16 字符是分钟桶，后面跟 "|"
		if len(k) > 16 && k[:16] <= cut {
			delete(s.perMin, k)
		}
	}
}

// Clear 清空全部缓冲（谨慎使用，仅用于整体重置）。
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.perMin = make(map[string]*Delta, 256)
	s.total = Delta{}
}

// Len 返回当前保留的分钟桶数。
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.perMin)
}

// LastSeen 返回最后一次记录时间。
func (s *Store) LastSeen() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeen
}

// Manager 按 backend 管理多个 Store。
type Manager struct {
	mu     sync.Mutex
	stores map[int64]*Store
	maxMin int
}

// NewManager 创建管理器。
func NewManager(maxMinute int) *Manager {
	return &Manager{stores: make(map[int64]*Store), maxMin: maxMinute}
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

// Prune 对所有 backend 执行裁剪。
func (m *Manager) Prune(cutoff time.Time) {
	m.mu.Lock()
	stores := make([]*Store, 0, len(m.stores))
	for _, st := range m.stores {
		stores = append(stores, st)
	}
	m.mu.Unlock()

	for _, st := range stores {
		st.Prune(cutoff)
	}
}

// Drop 移除某个 backend 的缓冲。
func (m *Manager) Drop(backendID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.stores, backendID)
}

// Overlay 把实时增量叠加到一份 SQLite 查询结果上。
//
// 输入是 stats 层的行（每行含 domain/ip/source_ip/chain/rule 与 upload/download/connections），
// 用 map 表示以便兼容不同的行类型。叠加逻辑：
//   - 若某条实时增量的键已存在于行中 → 累加到该行
//   - 否则追加为新行
//
// 返回新切片，不修改入参。
func Overlay(rows []map[string]any, dimKey []string, deltas []Delta) []map[string]any {
	if len(deltas) == 0 {
		return rows
	}

	index := make(map[string]int, len(rows))
	for i, r := range rows {
		index[rowKey(r, dimKey)] = i
	}

	out := make([]map[string]any, len(rows))
	copy(out, rows)

	for _, d := range deltas {
		cand := map[string]any{}
		row := deltaToRow(d)
		for _, k := range dimKey {
			if v, ok := row[k]; ok {
				cand[k] = v
			}
		}
		k := rowKey(cand, dimKey)

		if i, ok := index[k]; ok {
			addToRow(out[i], d)
			continue
		}
		newRow := map[string]any{}
		for kk, vv := range row {
			newRow[kk] = vv
		}
		out = append(out, newRow)
		index[k] = len(out) - 1
	}
	return out
}

func deltaToRow(d Delta) map[string]any {
	chain := d.FullChain
	if chain == "" {
		chain = d.Chain
	}
	return map[string]any{
		"domain":      d.Domain,
		"ip":          d.IP,
		"source_ip":   d.SourceIP,
		"sourceIP":    d.SourceIP,
		"chain":       chain,
		"rule":        d.Rule,
		"upload":      d.Upload,
		"download":    d.Download,
		"connections": d.Connections,
	}
}

func rowKey(r map[string]any, dimKey []string) string {
	if len(dimKey) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, k := range dimKey {
		sb.WriteString(anyToStr(r[k]))
		sb.WriteByte('|')
	}
	return sb.String()
}

func anyToStr(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	default:
		return ""
	}
}

func addToRow(r map[string]any, d Delta) {
	r["upload"] = asInt64(r["upload"]) + d.Upload
	r["download"] = asInt64(r["download"]) + d.Download
	r["connections"] = asInt64(r["connections"]) + d.Connections
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}
