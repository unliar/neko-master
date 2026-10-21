package realtime

import (
	"testing"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

func mkUpdate(dom, ip, rule string, up, down, conn int64, at time.Time) model.TrafficUpdate {
	return model.TrafficUpdate{
		TimestampMs: at, Domain: dom, IP: ip, SourceIP: "192.168.1.5",
		Chain: "PROXY", FullChain: "PROXY", Rule: rule,
		Upload: up, Download: down, Connections: conn,
	}
}

func TestRecordAndSnapshot(t *testing.T) {
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 10, 20, 1, base))
	s.Record(mkUpdate("b.com", "2.2.2.2", "Match", 5, 5, 1, base.Add(time.Minute)))

	snap := s.Snapshot()
	if snap.Upload != 15 || snap.Download != 25 || snap.Connections != 2 {
		t.Errorf("快照 = %+v，应为 {15 25 2}", snap)
	}
}

func TestSameMinuteSameKeyMerges(t *testing.T) {
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 10, 0, 1, base))
	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 3, 0, 0, base.Add(5*time.Second)))

	if s.Len() != 1 {
		t.Errorf("同分钟同键应合并为 1 桶，得到 %d", s.Len())
	}
	d := s.Deltas()[0]
	if d.Upload != 13 {
		t.Errorf("合并后 upload = %d，应为 13", d.Upload)
	}
}

func TestPruneDropsFlushedMinutes(t *testing.T) {
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 1, 1, 1, base))
	s.Record(mkUpdate("b.com", "2.2.2.2", "Match", 1, 1, 1, base.Add(2*time.Minute)))
	if s.Len() != 2 {
		t.Fatalf("应有 2 桶，得到 %d", s.Len())
	}

	// flush 到 02:01，裁剪 <= 02:01
	s.Prune(base.Add(time.Minute))
	if s.Len() != 1 {
		t.Errorf("裁剪后应剩 1 桶，得到 %d", s.Len())
	}
	// total 累计不应被裁剪影响（它代表「尚未落盘但已记录」的口径由调用方维护）
	snap := s.Snapshot()
	if snap.Upload != 2 {
		t.Errorf("total 不受 Prune 影响，得到 %d", snap.Upload)
	}
}

func TestPruneKeepsFutureMinutes(t *testing.T) {
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 1, 1, 1, base.Add(10*time.Minute)))

	s.Prune(base)
	if s.Len() != 1 {
		t.Errorf("未来分钟桶不应被裁掉，得到 %d", s.Len())
	}
}

func TestSinceFiltersByTime(t *testing.T) {
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 10, 0, 1, base))
	s.Record(mkUpdate("b.com", "2.2.2.2", "Match", 7, 0, 1, base.Add(3*time.Minute)))

	got := s.Since(base.Add(2 * time.Minute))
	if got.Upload != 7 {
		t.Errorf("Since(02:02) = %d，应只含 02:03 那条 7", got.Upload)
	}
}

func TestOverlayAddsToExistingRow(t *testing.T) {
	rows := []map[string]any{
		{"domain": "a.com", "upload": int64(100), "download": int64(200), "connections": int64(3)},
	}
	deltas := []Delta{{
		Domain: "a.com", Upload: 10, Download: 20, Connections: 1,
	}}

	out := Overlay(rows, []string{"domain"}, deltas)
	if len(out) != 1 {
		t.Fatalf("已存在的键应就地累加，行数应仍为 1，得到 %d", len(out))
	}
	if out[0]["upload"].(int64) != 110 {
		t.Errorf("upload = %v，应为 110", out[0]["upload"])
	}
	if out[0]["download"].(int64) != 220 {
		t.Errorf("download = %v，应为 220", out[0]["download"])
	}
}

func TestOverlayAppendsNewRow(t *testing.T) {
	rows := []map[string]any{
		{"domain": "a.com", "upload": int64(100), "download": int64(0), "connections": int64(1)},
	}
	deltas := []Delta{{
		Domain: "b.com", Upload: 5, Download: 5, Connections: 1,
	}}
	out := Overlay(rows, []string{"domain"}, deltas)
	if len(out) != 2 {
		t.Fatalf("新键应追加为 1 行，得到 %d", len(out))
	}
	// 原切片不能被污染
	if rows[0]["upload"].(int64) != 100 {
		t.Error("Overlay 不应修改入参切片")
	}
}

func TestOverlayNilAndEmpty(t *testing.T) {
	if got := Overlay(nil, []string{"domain"}, nil); got != nil {
		t.Error("无 delta 时应原样返回")
	}
	rows := []map[string]any{{"domain": "a", "upload": int64(1)}}
	if got := Overlay(rows, []string{"domain"}, nil); len(got) != 1 {
		t.Error("空 delta 应原样返回")
	}
}

func TestOverlayMultiDimKey(t *testing.T) {
	rows := []map[string]any{
		{"domain": "a.com", "chain": "P1", "upload": int64(10), "download": int64(0), "connections": int64(0)},
	}
	// 同 domain 不同 chain → 应追加而非累加
	deltas := []Delta{{Domain: "a.com", FullChain: "P2", Upload: 3, Connections: 1}}
	out := Overlay(rows, []string{"domain", "chain"}, deltas)
	if len(out) != 2 {
		t.Errorf("不同 chain 应为独立键，得到 %d 行", len(out))
	}
}

func TestManagerIsolatesBackends(t *testing.T) {
	m := NewManager(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	m.Record(1, mkUpdate("a.com", "1.1.1.1", "Match", 10, 0, 1, base))
	m.Record(2, mkUpdate("a.com", "1.1.1.1", "Match", 99, 0, 1, base))

	if m.Store(1).Snapshot().Upload != 10 {
		t.Errorf("backend 1 应独立，得到 %d", m.Store(1).Snapshot().Upload)
	}
	if m.Store(2).Snapshot().Upload != 99 {
		t.Errorf("backend 2 应独立，得到 %d", m.Store(2).Snapshot().Upload)
	}

	m.Drop(1)
	m.Prune(base.Add(time.Hour))
	if m.Store(2).Len() != 0 {
		t.Error("Prune 应作用于所有 backend")
	}
}

func TestFlushThenOverlayNoDoubleCount(t *testing.T) {
	// 关键一致性场景：落盘后裁剪 buffer，再叠加 → 不重复计数
	s := NewStore(180)
	base := time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 10, 20, 1, base))

	// 模拟 flush：SQLite 已含这 10/20，随后裁剪
	s.Prune(base)
	if s.Len() != 0 {
		t.Fatal("裁剪后 buffer 应为空")
	}
	// 新一轮流量
	s.Record(mkUpdate("a.com", "1.1.1.1", "Match", 5, 5, 0, base.Add(time.Minute)))

	rows := []map[string]any{
		{"domain": "a.com", "upload": int64(10), "download": int64(20), "connections": int64(1)},
	}
	out := Overlay(rows, []string{"domain"}, s.Deltas())
	if len(out) != 1 {
		t.Fatalf("应仍 1 行，得到 %d", len(out))
	}
	if out[0]["upload"].(int64) != 15 || out[0]["download"].(int64) != 25 {
		t.Errorf("叠加后 = %v/%v，应为 15/25（不重复计数）", out[0]["upload"], out[0]["download"])
	}
}
