package collector

import (
	"testing"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

func conn(id, host, ip, sourceIP string, up, down int64, chains []string, rule string, start time.Time) model.Connection {
	return model.Connection{
		ID: id,
		Metadata: model.ConnectionMetadata{
			Host:          host,
			DestinationIP: ip,
			SourceIP:      sourceIP,
		},
		Upload:   up,
		Download: down,
		Chains:   chains,
		Rule:     rule,
		Start:    start.Format(time.RFC3339Nano),
	}
}

func snap(conns ...model.Connection) *model.ConnectionsResponse {
	return &model.ConnectionsResponse{Connections: conns}
}

// 首次快照的连接只建基线，不计流量（preexisting 宽限）。
func TestDiff_FirstSightingNoTraffic(t *testing.T) {
	tr := NewTracker()
	now := time.Now()

	// 连接建立于 1 分钟前 → preexisting，只建基线
	c := conn("1", "example.com", "1.2.3.4", "192.168.1.10",
		1000, 2000, []string{"PROXY"}, "DomainSuffix", now.Add(-time.Minute))

	got := tr.Diff(snap(c), now)
	if len(got) != 0 {
		t.Fatalf("首次快照应无增量，实际 %d 条", len(got))
	}
	if tr.ActiveCount() != 1 {
		t.Fatalf("应追踪 1 条连接，实际 %d", tr.ActiveCount())
	}
}

// 第二轮快照应算出与上一轮的差。
func TestDiff_SecondSightingProducesDelta(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	start := now.Add(-time.Minute)

	tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 1000, 2000, []string{"PROXY"}, "DomainSuffix", start)), now)

	now2 := now.Add(2 * time.Second)
	got := tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 1500, 2600, []string{"PROXY"}, "DomainSuffix", start)), now2)

	if len(got) != 1 {
		t.Fatalf("应产生 1 条增量，实际 %d", len(got))
	}
	if got[0].Upload != 500 {
		t.Errorf("upload 增量应为 500，实际 %d", got[0].Upload)
	}
	if got[0].Download != 600 {
		t.Errorf("download 增量应为 600，实际 %d", got[0].Download)
	}
	if got[0].Connections != 1 {
		t.Errorf("首次计流量应计 1 个连接，实际 %d", got[0].Connections)
	}
}

// 同一条连接后续增量不再计连接数。
func TestDiff_ConnectionCountedOnlyOnce(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	start := now.Add(-time.Minute)

	tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 100, 200, []string{"P"}, "R", start)), now)
	tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 200, 300, []string{"P"}, "R", start)), now.Add(time.Second))
	got := tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 300, 400, []string{"P"}, "R", start)), now.Add(2*time.Second))

	if len(got) != 1 {
		t.Fatalf("应产生 1 条增量，实际 %d", len(got))
	}
	if got[0].Connections != 0 {
		t.Errorf("后续增量不应再计连接数，实际 %d", got[0].Connections)
	}
}

// 计数器回退时把当前值整体当增量。
func TestDiff_CounterReset(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	start := now.Add(-time.Minute)

	tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 5000, 9000, []string{"P"}, "R", start)), now)
	// 回退到 100
	got := tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 100, 200, []string{"P"}, "R", start)), now.Add(time.Second))

	if len(got) != 1 {
		t.Fatalf("计数器回退应产生增量，实际 %d 条", len(got))
	}
	if got[0].Upload != 100 || got[0].Download != 200 {
		t.Errorf("回退应取当前值，实际 up=%d down=%d", got[0].Upload, got[0].Download)
	}
}

// 连接消失后从追踪表移除。
func TestDiff_RemovesGoneConnections(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	start := now.Add(-time.Minute)

	tr.Diff(snap(
		conn("1", "a.com", "1.1.1.1", "10.0.0.1", 0, 0, []string{"P"}, "R", start),
		conn("2", "b.com", "2.2.2.2", "10.0.0.2", 0, 0, []string{"P"}, "R", start),
	), now)
	if tr.ActiveCount() != 2 {
		t.Fatalf("应追踪 2 条，实际 %d", tr.ActiveCount())
	}

	tr.Diff(snap(conn("1", "a.com", "1.1.1.1", "10.0.0.1", 0, 0, []string{"P"}, "R", start)), now.Add(time.Second))
	if tr.ActiveCount() != 1 {
		t.Fatalf("消失的连接应被移除，实际剩 %d", tr.ActiveCount())
	}
}

// 零增量不产生记录。
func TestDiff_ZeroDeltaSkipped(t *testing.T) {
	tr := NewTracker()
	now := time.Now()
	start := now.Add(-time.Minute)

	c := conn("1", "a.com", "1.1.1.1", "10.0.0.1", 100, 200, []string{"P"}, "R", start)
	tr.Diff(snap(c), now)
	got := tr.Diff(snap(c), now.Add(time.Second)) // 累计值没变

	if len(got) != 0 {
		t.Fatalf("零增量不应产生记录，实际 %d 条", len(got))
	}
}

// 链路拼接。
func TestJoinChain(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "DIRECT"},
		{[]string{}, "DIRECT"},
		{[]string{"A"}, "A"},
		{[]string{"A", "B", "C"}, "A > B > C"},
	}
	for _, c := range cases {
		if got := joinChain(c.in); got != c.want {
			t.Errorf("joinChain(%v) = %q, 期望 %q", c.in, got, c.want)
		}
	}
}

// 分桶键必须是 UTC。
func TestBucketKeys(t *testing.T) {
	// 2026-10-21 16:05:30 UTC
	tm := time.Date(2026, 10, 21, 16, 5, 30, 0, time.UTC)
	if got := minuteKey(tm); got != "2026-10-21T16:05:00" {
		t.Errorf("minuteKey = %q", got)
	}
	if got := hourKey(tm); got != "2026-10-21T16:00:00" {
		t.Errorf("hourKey = %q", got)
	}

	// 本地时区也必须转成 UTC
	cst := time.FixedZone("CST", 8*3600)
	tm2 := time.Date(2026, 10, 22, 0, 5, 30, 0, cst) // = 2026-10-21T16:05:30Z
	if got := minuteKey(tm2); got != "2026-10-21T16:05:00" {
		t.Errorf("非 UTC 输入应转换，得到 %q", got)
	}
}
