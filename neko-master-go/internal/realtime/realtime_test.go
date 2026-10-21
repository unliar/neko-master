package realtime

import (
	"testing"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

func mkUpdate(dom, rule string, up, down, conn int64, at time.Time) model.TrafficUpdate {
	return model.TrafficUpdate{
		TimestampMs: at, Domain: dom, IP: "1.1.1.1", SourceIP: "192.168.1.5",
		Chain: "PROXY", FullChain: "PROXY", Rule: rule,
		Upload: up, Download: down, Connections: conn,
	}
}

var base = time.Date(2026, 10, 22, 2, 30, 0, 0, time.UTC)

// ---- \u5bf9\u9f50 recordTraffic ----

// \u65e9\u9000\uff1aupload \u4e0e download \u90fd <= 0 \u65f6\u76f4\u63a5\u8fd4\u56de\uff0c\u4e0d\u5efa minute \u6876\u3002
func TestRecordEarlyReturnZeroTraffic(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 0, 0, 1, base))
	if s.Len() != 0 {
		t.Errorf("\u96f6\u6d41\u91cf\u4e0d\u5e94\u5efa minute \u6876\uff0c\u5f97\u5230 %d", s.Len())
	}
	d := s.SummaryDelta()
	if d.Upload != 0 || d.Download != 0 || d.Connections != 0 {
		t.Errorf("summary \u5e94\u4e3a\u96f6\uff0c\u5f97\u5230 %+v", d)
	}
}

// \u8fde\u63a5\u6570\u6309\u8c03\u7528\u65b9\u7ed9\u7684\u503c\u7d2f\u52a0\uff0c\u4e0d\u5f3a\u5236\u9ed8\u8ba4 1\u3002
func TestRecordConnectionsAsGiven(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 10, 20, 1, base))
	s.Record(mkUpdate("a.com", "Match", 5, 5, 0, base.Add(time.Second)))

	d := s.SummaryDelta()
	if d.Connections != 1 {
		t.Errorf("connections \u5e94\u4e3a 1\uff081+0\uff09\uff0c\u5f97\u5230 %d", d.Connections)
	}
	if d.Upload != 15 || d.Download != 25 {
		t.Errorf("summary = %+v\uff0c\u5e94\u4e3a {15 25}", d)
	}
}

// summary \u662f\u7d2f\u52a0\u5668\uff1a\u53ea\u589e\u4e0d\u51cf\u3002
func TestSummaryAccumulates(t *testing.T) {
	s := NewStore(180)
	for i := 0; i < 3; i++ {
		s.Record(mkUpdate("a.com", "Match", 10, 10, 1, base.Add(time.Duration(i)*time.Minute)))
	}
	d := s.SummaryDelta()
	if d.Upload != 30 || d.Download != 30 || d.Connections != 3 {
		t.Errorf("summary = %+v\uff0c\u5e94\u4e3a {30 30 3}", d)
	}
}

// ---- \u5bf9\u9f50 getTodayDelta ----

func TestTodayDeltaFiltersByUTCDate(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 10, 10, 1, base))
	// \u524d\u4e00\u5929\uff08\u65e9\u4e8e\u4eca\u65e5 2:30\uff09
	s.Record(mkUpdate("b.com", "Match", 7, 7, 1, base.Add(-24*time.Hour)))

	up, down := s.TodayDelta(base)
	if up != 10 || down != 10 {
		t.Errorf("\u4eca\u65e5\u589e\u91cf = %d/%d\uff0c\u5e94\u53ea\u542b 10/10", up, down)
	}

	// 跳到下一天查：旧桶已超出 maxMinutes 滑动窗口（默认 180 分）
	// 而被 prune 剪掉，因此只能得到今日那条。这正是
	// prune 与 getTodayDelta 交互的真实行为。
	up2, _ := s.TodayDelta(base.Add(24 * time.Hour))
	if up2 != 0 {
		t.Errorf("查 10-23 时今日桶不应匹配，应为 0，得到 %d", up2)
	}
}

func TestTodayDeltaEmpty(t *testing.T) {
	s := NewStore(180)
	up, down := s.TodayDelta(base)
	if up != 0 || down != 0 {
		t.Errorf("\u7a7a\u7f13\u51b2\u5e94\u8fd4\u56de 0\uff0c\u5f97\u5230 %d/%d", up, down)
	}
}

// ---- \u5bf9\u9f50 clearTraffic* ----

func TestClearTrafficResetsSummary(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 10, 10, 1, base))

	s.ClearTraffic()

	d := s.SummaryDelta()
	if d.Upload != 0 || d.Download != 0 || d.Connections != 0 {
		t.Errorf("clear \u540e summary \u5e94\u4e3a\u96f6\uff0c\u5f97\u5230 %+v", d)
	}
	if s.Len() != 0 {
		t.Errorf("clear \u540e minute \u6876\u5e94\u4e3a\u96f6\uff0c\u5f97\u5230 %d", s.Len())
	}
}

// \u5173\u952e\u4e00\u81f4\u6027\uff1a\u843d\u76d8\u6210\u529f \u2192 clear\uff0c\u7136\u540e\u53e0\u52a0\u4e0d\u4f1a\u91cd\u590d\u8ba1\u6570\u3002
func TestClearThenRecordNoDoubleCount(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 100, 200, 1, base))

	// SQLite \u5df2\u5305\u542b\u8fd9 100/200\uff0cflush \u6210\u529f\u540e clear
	s.ClearTraffic()

	// \u4e0b\u4e00\u8f6e\u65b0\u589e\u91cf
	s.Record(mkUpdate("a.com", "Match", 5, 5, 0, base.Add(time.Minute)))

	d := s.SummaryDelta()
	if d.Upload != 5 || d.Download != 5 {
		t.Errorf("clear \u540e\u53e0\u52a0 = %+v\uff0c\u5e94\u4e3a {5 5}\uff08\u4e0d\u91cd\u590d\u8ba1\u6570\uff09", d)
	}
}

// \u843d\u76d8\u5931\u8d25\u65f6\u4e0d clear \u2192 \u4e0b\u6b21 flush \u5e26\u7740\u65e7\u6570\u636e\u91cd\u8bd5\uff0c\u6570\u636e\u4e0d\u4e22\u3002
func TestNoClearOnFailureRetainsData(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 100, 200, 1, base))

	// \u6a21\u62df flush \u5931\u8d25\uff1a\u4e0d\u8c03 ClearTraffic
	// \u4e0b\u4e00\u8f6e\u7ee7\u7eed\u7d2f\u52a0
	s.Record(mkUpdate("a.com", "Match", 5, 5, 0, base.Add(time.Minute)))

	d := s.SummaryDelta()
	if d.Upload != 105 || d.Download != 205 {
		t.Errorf("\u5931\u8d25\u540e\u5e94\u4fdd\u7559\u5168\u90e8\u6570\u636e\u4f9b\u91cd\u8bd5\uff0c\u5f97\u5230 %+v\uff0c\u5e94\u4e3a {105 205}", d)
	}
}

// ---- \u5bf9\u9f50 pruneOldBuckets ----

// prune \u53ea\u52a8 minute \u6876\uff0c\u4e0d\u6e05 summary \u7d2f\u52a0\u5668\u3002
func TestPruneDoesNotTouchSummary(t *testing.T) {
	s := NewStore(3) // maxMinutes = 3
	old := base.Add(-10 * time.Minute)
	s.Record(mkUpdate("a.com", "Match", 10, 10, 1, old))
	s.Record(mkUpdate("b.com", "Match", 5, 5, 1, base))

	// \u65e7\u6876\u8d85\u51fa\u6ed1\u52a8\u7a97\u53e3\uff0c\u5e94\u88ab\u526a\u6389
	if s.Len() != 1 {
		t.Errorf("prune \u540e\u5e94\u53ea\u5269 1 \u6876\uff0c\u5f97\u5230 %d", s.Len())
	}
	// \u4f46 summary \u4f9d\u7136\u4fdd\u7559\u5168\u90e8
	d := s.SummaryDelta()
	if d.Upload != 15 || d.Download != 15 {
		t.Errorf("prune \u4e0d\u5e94\u5f71\u54cd summary\uff0c\u5f97\u5230 %+v\uff0c\u5e94\u4e3a {15 15}", d)
	}
}

// \u672a\u6765\u65f6\u95f4\u6876\u4e0d\u4f1a\u88ab\u526a\uff08\u9632\u65f6\u949f\u504f\u79fb\u5bfc\u81f4\u9519\u526a\uff09\u3002
func TestPruneKeepsFutureBuckets(t *testing.T) {
	s := NewStore(3)
	s.Record(mkUpdate("a.com", "Match", 1, 1, 1, base.Add(10*time.Minute)))
	s.Record(mkUpdate("b.com", "Match", 1, 1, 1, base))

	if s.Len() != 2 {
		t.Errorf("\u672a\u6765\u6876\u4e0d\u5e94\u88ab\u526a\uff0c\u5f97\u5230 %d", s.Len())
	}
}

func TestMinuteKeyFormat(t *testing.T) {
	// \u5bf9\u9f50\u4e0a\u6e38 toMinuteKey\uff1aISO \u524d 16 \u4f4d + ":00"
	got := MinuteKeyForTest(base)
	if got != "2026-10-22T02:30:00" {
		t.Errorf("minuteKey = %q\uff0c\u5e94\u4e3a 2026-10-22T02:30:00", got)
	}
}

func TestClearTrafficDimensionsIsSafeNoop(t *testing.T) {
	s := NewStore(180)
	s.Record(mkUpdate("a.com", "Match", 10, 10, 1, base))
	s.ClearTrafficDimensions() // \u4e0d\u5e94 panic\uff0c\u4e5f\u4e0d\u5e94\u6e05 summary
	d := s.SummaryDelta()
	if d.Upload != 10 {
		t.Errorf("ClearTrafficDimensions \u4e0d\u5e94\u6e05 summary\uff0c\u5f97\u5230 %d", d.Upload)
	}
}

// ---- Manager ----

func TestManagerIsolatesBackends(t *testing.T) {
	m := NewManager(180)
	m.Record(1, mkUpdate("a.com", "Match", 10, 0, 1, base))
	m.Record(2, mkUpdate("a.com", "Match", 99, 0, 1, base))

	if m.Store(1).SummaryDelta().Upload != 10 {
		t.Errorf("backend 1 \u5e94\u72ec\u7acb\uff0c\u5f97\u5230 %d", m.Store(1).SummaryDelta().Upload)
	}
	if m.Store(2).SummaryDelta().Upload != 99 {
		t.Errorf("backend 2 \u5e94\u72ec\u7acb\uff0c\u5f97\u5230 %d", m.Store(2).SummaryDelta().Upload)
	}

	m.ClearTraffic(1)
	if m.Store(1).SummaryDelta().Upload != 0 {
		t.Error("ClearTraffic(1) \u5e94\u6e05 backend 1")
	}
	if m.Store(2).SummaryDelta().Upload != 99 {
		t.Error("ClearTraffic(1) \u4e0d\u5e94\u5f71\u54cd backend 2")
	}

	m.Drop(1)
	m.Drop(2)
}

func TestManagerDropThenRecreate(t *testing.T) {
	m := NewManager(180)
	m.Record(1, mkUpdate("a.com", "Match", 10, 0, 1, base))
	m.Drop(1)
	// \u91cd\u5efa\u540e\u5e94\u4e3a\u7a7a
	if m.Store(1).SummaryDelta().Upload != 0 {
		t.Error("Drop \u540e\u91cd\u5efa\u5e94\u4e3a\u7a7a")
	}
}
