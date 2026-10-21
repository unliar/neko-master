package collector

import (
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// minuteKey 生成 UTC 分钟桶键：YYYY-MM-DDTHH:MM:00
func minuteKey(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04") + ":00"
}

// hourKey 生成 UTC 小时桶键：YYYY-MM-DDTHH:00:00
func hourKey(t time.Time) string {
	return t.UTC().Format("2006-01-02T15") + ":00:00"
}

// bufferKey 是窗口内聚合的去重键。
type bufferKey struct {
	minute   string
	domain   string
	ip       string
	sourceIP string
	chain    string
	rule     string
}

// Buffer 在内存中按分钟窗口累加增量，减少 UPSERT 次数。
//
// 对应上游 batch-buffer.ts 的层 1 聚合。
type Buffer struct {
	mu      sync.Mutex
	backend int64
	entries map[bufferKey]*model.TrafficUpdate
	maxSize int
}

// NewBuffer 创建缓冲区。maxSize <= 0 时默认 5000。
func NewBuffer(backendID int64, maxSize int) *Buffer {
	if maxSize <= 0 {
		maxSize = 5000
	}
	return &Buffer{
		backend: backendID,
		entries: make(map[bufferKey]*model.TrafficUpdate, 256),
		maxSize: maxSize,
	}
}

// Add 累加一条增量。返回 true 表示已达到刷盘水位。
func (b *Buffer) Add(u model.TrafficUpdate) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	k := bufferKey{
		minute:   minuteKey(u.TimestampMs),
		domain:   u.Domain,
		ip:       u.IP,
		sourceIP: u.SourceIP,
		chain:    u.FullChain,
		rule:     u.Rule,
	}
	if e, ok := b.entries[k]; ok {
		e.Upload += u.Upload
		e.Download += u.Download
		e.Connections += u.Connections
		if u.TimestampMs.After(e.TimestampMs) {
			e.TimestampMs = u.TimestampMs
		}
	} else {
		cp := u
		b.entries[k] = &cp
	}
	return len(b.entries) >= b.maxSize
}

// Drain 取出并清空当前所有条目。
func (b *Buffer) Drain() []model.TrafficUpdate {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.entries) == 0 {
		return nil
	}
	out := make([]model.TrafficUpdate, 0, len(b.entries))
	for _, e := range b.entries {
		out = append(out, *e)
	}
	b.entries = make(map[bufferKey]*model.TrafficUpdate, 256)
	return out
}

// Len 返回当前缓冲条目数。
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// BackendID 返回所属 backend。
func (b *Buffer) BackendID() int64 { return b.backend }
