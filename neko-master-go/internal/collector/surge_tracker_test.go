package collector

import (
	"testing"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
)

func boolPtr(b bool) *bool        { return &b }
func floatPtr(f float64) *float64 { return &f }

func mkReq(id, host string, in, out float64, at float64) gateway.SurgeRequest {
	return gateway.SurgeRequest{
		ID:         id,
		RemoteHost: host,
		InBytes:    in,
		OutBytes:   out,
		Time:       at,
		PolicyName: "FinalProxy",
		Rule:       "DOMAIN-SUFFIX",
	}
}

func TestSurgeFirstSnapshotNoStartDateCountsInitial(t *testing.T) {
	// 上游 isPreexistingConnection：startDate 无法解析时返回 false，
	// 因此首次就带字节的请求会把当前值当增量计一次。
	tr := NewSurgeTracker()
	now := time.Unix(1761032400, 0).UTC()
	data := &gateway.SurgeRequestsData{
		Requests: []gateway.SurgeRequest{mkReq("r1", "a.com", 100, 50, 1761032400)},
	}
	ups := tr.Diff(data, now)
	if len(ups) != 1 {
		t.Fatalf("首次带字节应计 1 条，得到 %d", len(ups))
	}
	if ups[0].Upload != 50 || ups[0].Download != 100 {
		t.Errorf("初始量 = up %d / down %d，应为 50/100", ups[0].Upload, ups[0].Download)
	}
	if ups[0].Connections != 1 {
		t.Errorf("首次应计 1 连接，得到 %d", ups[0].Connections)
	}
}

// TestSurgePreexistingOnlyBaselines: startDate 早于开始时刻 5s 以上
// → preexisting → 只建基线，不计量。
func TestSurgePreexistingOnlyBaselines(t *testing.T) {
	tr := NewSurgeTracker()
	now := time.Unix(1761032400, 0).UTC()
	r := mkReq("r1", "a.com", 100, 50, 1761032390)
	r.StartDate = floatPtr(1761032300)
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r}}, now)
	if len(ups) != 0 {
		t.Fatalf("preexisting 不应计量，得到 %d 条", len(ups))
	}
	if tr.ActiveCount() != 1 {
		t.Errorf("仍应建基线，得到 %d", tr.ActiveCount())
	}
}

func TestSurgeDeltaComputed(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 100, 50, base),
	}}, now)

	// 第二轮：in 200（下载），out 80（上传）
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 200, 80, base+1),
	}}, now.Add(time.Second))

	if len(ups) != 1 {
		t.Fatalf("应有 1 条增量，得到 %d", len(ups))
	}
	u := ups[0]
	if u.Download != 100 {
		t.Errorf("下载增量 = %d，应为 100", u.Download)
	}
	if u.Upload != 30 {
		t.Errorf("上传增量 = %d，应为 30", u.Upload)
	}
	// 首轮已计过 1 次连接数，此处不应重复
	if u.Connections != 0 {
		t.Errorf("连接数只计一次，第二轮应为 0，得到 %d", u.Connections)
	}
}

func TestSurgeConnectionCountedOnce(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	// 首轮带字节 → 计 1 次连接数
	u0 := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 100, 50, base),
	}}, now)
	if len(u0) != 1 || u0[0].Connections != 1 {
		t.Fatalf("首轮应计 1 连接，得到 %d 条", len(u0))
	}

	u1 := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 200, 80, base+1),
	}}, now.Add(time.Second))
	u2 := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 300, 110, base+2),
	}}, now.Add(2*time.Second))

	if u1[0].Connections != 0 {
		t.Errorf("第二轮连接数应为 0，得到 %d", u1[0].Connections)
	}
	if u2[0].Connections != 0 {
		t.Errorf("后续轮次连接数应为 0，得到 %d", u2[0].Connections)
	}
}

func TestSurgeCompletedNotRecounted(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	req := mkReq("r1", "a.com", 100, 50, base)
	req.Completed = boolPtr(true)
	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{req}}, now)

	if tr.CompletedCount() != 1 {
		t.Fatalf("完成应被记录，得到 %d", tr.CompletedCount())
	}

	// Surge 在后续轮询仍返回同一请求，字节未增 → 必须跳过
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{req}},
		now.Add(2*time.Second))
	if len(ups) != 0 {
		t.Errorf("已完成且无新增字节应跳过，得到 %d 条", len(ups))
	}

	// 竟然又涨了 → 解除完成标记重新处理
	req2 := req
	req2.InBytes = 150
	ups2 := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{req2}},
		now.Add(3*time.Second))
	if len(ups2) != 1 || ups2[0].Download != 50 {
		t.Errorf("新字节应被处理，得到 %d 条: %+v", len(ups2), ups2)
	}
}

func TestSurgeCounterRegression(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 1000, 500, base),
	}}, now)

	// 计数器回退（id 复用）
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 5, 3, base+1),
	}}, now.Add(time.Second))
	if len(ups) != 1 {
		t.Fatalf("应产生 1 条，得到 %d", len(ups))
	}
	if ups[0].Upload != 3 || ups[0].Download != 5 {
		t.Errorf("回退应整体计为增量，得到 up=%d down=%d", ups[0].Upload, ups[0].Download)
	}
}

func TestSurgeChainFromNotes(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	r1 := mkReq("r1", "a.com", 10, 10, base)
	r1.Notes = []string{"[Rule] Policy decision path: DOMAIN-SUFFIX -> Auto -> HK-Node"}
	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r1}}, now)

	r2 := r1
	r2.InBytes = 20
	r2.OutBytes = 20
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r2}}, now.Add(time.Second))

	if len(ups) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(ups))
	}
	if ups[0].Chain != "HK-Node" {
		t.Errorf("出口应为 HK-Node，得到 %q", ups[0].Chain)
	}
	if ups[0].FullChain != "HK-Node > Auto > DOMAIN-SUFFIX" {
		t.Errorf("完整链路 = %q", ups[0].FullChain)
	}
}

func TestSurgeStaleCleanup(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()
	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{
		mkReq("r1", "a.com", 10, 10, base),
	}}, now)

	// 超过 5 分钟未再出现，下一轮空快照应清掉
	tr.Diff(&gateway.SurgeRequestsData{}, now.Add(6*time.Minute))
	if tr.ActiveCount() != 0 {
		t.Errorf("过期请求应被清理，剩 %d", tr.ActiveCount())
	}
}

func TestSurgeSourceIPPropagated(t *testing.T) {
	tr := NewSurgeTracker()
	base := float64(1761032400)
	now := time.Unix(1761032400, 0).UTC()

	r := mkReq("r1", "a.com", 10, 10, base)
	r.SourceAddress = "192.168.1.50:5555"
	r.LocalAddress = "192.168.1.1:9090"
	tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r}}, now)

	r2 := r
	r2.InBytes, r2.OutBytes = 20, 20
	ups := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r2}}, now.Add(time.Second))
	if len(ups) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(ups))
	}
	if ups[0].SourceIP != "192.168.1.50" {
		t.Errorf("sourceIP = %q", ups[0].SourceIP)
	}
}

func TestSurgeNilAndEmpty(t *testing.T) {
	tr := NewSurgeTracker()
	if got := tr.Diff(nil, time.Now()); got != nil {
		t.Error("nil 数据应返回 nil")
	}
	if got := tr.Diff(&gateway.SurgeRequestsData{}, time.Now()); got != nil {
		t.Error("空请求应返回 nil")
	}
}

func TestSurgeSkipsEmptyID(t *testing.T) {
	tr := NewSurgeTracker()
	r := mkReq("", "a.com", 10, 10, 1761032400)
	got := tr.Diff(&gateway.SurgeRequestsData{Requests: []gateway.SurgeRequest{r}},
		time.Unix(1761032400, 0).UTC())
	if len(got) != 0 {
		t.Errorf("无 id 的请求应被跳过，得到 %d", len(got))
	}
	if tr.ActiveCount() != 0 {
		t.Errorf("不应建立基线，得到 %d", tr.ActiveCount())
	}
}
