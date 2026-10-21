package clickhouse

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEncodeTrafficRow(t *testing.T) {
	rows := [][]any{
		{uint32(1), "2026-10-22 02:43:00", "example.com", "1.2.3.4", "192.168.1.5",
			"PROXY > DIRECT", "DOMAIN-SUFFIX", uint64(100), uint64(200), uint32(1)},
	}
	out, err := encodeJSONEachRow("traffic_detail_buffer", rows)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(out))

	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("非法 JSON: %v\n%s", err, line)
	}
	if m["backend_id"].(float64) != 1 {
		t.Errorf("backend_id = %v，应为数字 1", m["backend_id"])
	}
	if m["upload"].(float64) != 100 {
		t.Errorf("upload = %v，应为数字 100", m["upload"])
	}
	if m["connections"].(float64) != 1 {
		t.Errorf("connections = %v，应为数字 1", m["connections"])
	}
	if m["minute"] != "2026-10-22 02:43:00" {
		t.Errorf("minute = %v", m["minute"])
	}
	if m["chain"] != "PROXY > DIRECT" {
		t.Errorf("chain = %v", m["chain"])
	}
}

func TestEncodeEscapesQuotes(t *testing.T) {
	rows := [][]any{
		{uint32(1), "2026-10-22 02:43:00", `a"b\c`, "", "", "", "",
			uint64(0), uint64(0), uint32(0)},
	}
	out, _ := encodeJSONEachRow("traffic_detail_buffer", rows)
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &m); err != nil {
		t.Fatalf("转义失败: %v\n%s", err, out)
	}
	if m["domain"] != `a"b\c` {
		t.Errorf("domain = %q", m["domain"])
	}
}

func TestEncodeCountryAndAgg(t *testing.T) {
	crows := [][]any{
		{uint32(2), "2026-10-22 02:43:00", "CN", "China", "Asia", uint64(5), uint64(6), uint32(2)},
	}
	out, _ := encodeJSONEachRow("country_buffer", crows)
	var c map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &c); err != nil {
		t.Fatal(err)
	}
	if c["country"] != "CN" || c["continent"] != "Asia" {
		t.Errorf("country row = %v", c)
	}

	arows := [][]any{
		{uint32(2), "2026-10-22 02:43:00", uint64(5), uint64(6), uint32(2)},
	}
	out2, _ := encodeJSONEachRow("traffic_agg_buffer", arows)
	var a map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out2))), &a); err != nil {
		t.Fatal(err)
	}
	if len(a) != 5 {
		t.Errorf("agg 应有 5 列，得到 %d: %v", len(a), a)
	}
}

func TestMultiRowSeparatedByNewline(t *testing.T) {
	rows := [][]any{
		{uint32(1), "2026-10-22 02:43:00", "a", "", "", "", "", uint64(1), uint64(1), uint32(1)},
		{uint32(1), "2026-10-22 02:43:00", "b", "", "", "", "", uint64(1), uint64(1), uint32(1)},
	}
	out, _ := encodeJSONEachRow("traffic_detail_buffer", rows)
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("应 2 行，得到 %d", len(lines))
	}
	for i, l := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Errorf("第 %d 行非法: %v", i, err)
		}
	}
}

func TestClamp(t *testing.T) {
	if clampU64(-5) != 0 {
		t.Error("负数应 clamp 到 0")
	}
	if clampU32(-1) != 0 {
		t.Error("负数应 clamp 到 0")
	}
	if clampU32(1<<40) != 4294967295 {
		t.Error("溢出应 clamp 到 maxUint32")
	}
	if clampU64(1<<40) != 1<<40 {
		t.Error("正常值不应被改")
	}
}

func TestDisabledByDefault(t *testing.T) {
	w := NewWriter(LoadConfig(func(string, string) string { return "" }))
	if w != nil {
		t.Error("CH_ENABLED 未设置时应返回 nil Writer")
	}
}

func TestWriteTrafficAggregatesByMinute(t *testing.T) {
	base := time.Date(2026, 10, 22, 2, 43, 17, 0, time.UTC)
	ups := []struct {
		Up, Down, Conn int64
	}{
		{10, 20, 1},
		{5, 5, 1},
	}
	aggIdx := map[string]int{}
	var agg [][]any
	for _, u := range ups {
		k := base.Truncate(time.Minute).Format("2006-01-02 15:04:05")
		if i, ok := aggIdx[k]; ok {
			agg[i][2] = agg[i][2].(uint64) + clampU64(u.Up)
			agg[i][3] = agg[i][3].(uint64) + clampU64(u.Down)
			agg[i][4] = agg[i][4].(uint32) + clampU32(u.Conn)
		} else {
			aggIdx[k] = len(agg)
			agg = append(agg, []any{uint32(1), k, clampU64(u.Up), clampU64(u.Down), clampU32(u.Conn)})
		}
	}
	if len(agg) != 1 {
		t.Fatalf("同一分钟应合并为 1 行，得到 %d", len(agg))
	}
	if agg[0][2].(uint64) != 15 || agg[0][3].(uint64) != 25 || agg[0][4].(uint32) != 2 {
		t.Errorf("聚合结果错误: %v", agg[0])
	}
	if agg[0][1].(string) != "2026-10-22 02:43:00" {
		t.Errorf("分钟桶应截断到分，得到 %v", agg[0][1])
	}
}
