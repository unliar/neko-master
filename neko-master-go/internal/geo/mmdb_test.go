package geo

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// buildTestMMDB 在内存里构造一个最小 MMDB。
//
// 树结构（record_size=24）：
//
//	node0: left=1, right=2
//	node1: left=数据A, right=数据B
//
// 含义：
//
//	首位 0 -> 进入 node0.left = node1，再按次位分：
//	   次位 0 -> 数据A（US / Ashburn）
//	   次位 1 -> 数据B（JP）
//	首位 1 -> node0.right = node2（超出 nodeCount=2，按 node1 处理）
//
// 简化后：0.x.x.x → US，1.x.x.x → JP
func buildTestMMDB() []byte {
	encStr := func(s string) []byte {
		b := []byte(s)
		return append([]byte{(2 << 5) | byte(len(b))}, b...)
	}
	// pairs 传的是 [key1, val1, key2, val2, ...] 扁平切片，
	// MMDB map 头里的 size 是「键值对数量」= len(pairs)/2，
	// 不是 slice 个数。写成 len(pairs) 会让 size 翻倍，解码器按此数量
	// 读取时读到非字符串 key，直接判定解析失败。
	encMap := func(pairs [][]byte) []byte {
		if len(pairs)%2 != 0 {
			panic("encMap 需要偶数个元素（key/val 交替）")
		}
		out := []byte{(7 << 5) | byte(len(pairs)/2)}
		for _, p := range pairs {
			out = append(out, p...)
		}
		return out
	}

	dataA := encMap([][]byte{
		encStr("country"),
		encMap([][]byte{
			encStr("iso_code"), encStr("US"),
			encStr("names"), encMap([][]byte{encStr("en"), encStr("United States")}),
		}),
		encStr("city"),
		encMap([][]byte{encStr("names"), encMap([][]byte{encStr("en"), encStr("Ashburn")})}),
	})
	dataB := encMap([][]byte{
		encStr("country"),
		encMap([][]byte{
			encStr("iso_code"), encStr("JP"),
			encStr("names"), encMap([][]byte{encStr("en"), encStr("Japan")}),
		}),
	})

	rec := func(v uint32) []byte {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, v)
		return b[1:] // 24 bit -> 取低 3 字节
	}

	// 按首字节（前 8 位）线性分流的树：
	//   0..127  -> dataA (US)
	//   128..255 -> dataB (JP)
	//
	// 节点布局（nodeCount=2）：
	//   node0: left=1(node1), right=2(dataA)      首位 0 继续走，首位 1 直接 US
	//   node1: left=2(dataA), right=3(dataB)      次位 0 -> US，次位 1 -> JP
	//
	// 判定（首字节 b 的 bit7, bit6）：
	//   10.. 11..  -> 8.x / 12..  US
	//   00..       -> 0..127     US
	//   01..       -> 64..127    JP
	const nodeCount = 2
	node0 := append(rec(1), rec(nodeCount+0)...)
	node1 := append(rec(nodeCount+0), rec(nodeCount+1)...)
	tree := append(node0, node1...)

	dbType := []byte("Test-DB")
	descs := []byte{0x00, 0x01, 0x00, 0x02, 'e', 'n'}

	meta := make([]byte, 0, 64)
	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], nodeCount)
	meta = append(meta, u32[:]...)
	meta = append(meta, 0x00, 24) // record_size
	binary.BigEndian.PutUint32(u32[:], 4)
	meta = append(meta, u32[:]...) // ip_version
	meta = append(meta, byte(len(dbType)>>8), byte(len(dbType)))
	meta = append(meta, dbType...)
	meta = append(meta, 0x00, 0x00) // languages
	meta = append(meta, descs...)
	binary.BigEndian.PutUint32(u32[:], nodeCount)
	meta = append(meta, u32[:]...) // ipv4_node_count

	out := []byte{0xab}
	out = append(out, []byte("MaxMind.com")...)
	out = append(out, 0x00)
	out = append(out, meta...)
	out = append(out, tree...)
	out = append(out, dataA...)
	out = append(out, dataB...)
	return out
}

// ---------------------------------------------------------------- 魔数

func TestIsMMDB(t *testing.T) {
	if !isMMDB(buildTestMMDB()) {
		t.Fatal("合法 MMDB 应通过魔数校验")
	}
	if isMMDB([]byte("not an mmdb")) {
		t.Fatal("随机数据不应通过魔数校验")
	}
	if isMMDB([]byte{0xab, 'M'}) {
		t.Fatal("过短数据不应通过")
	}
}

// ---------------------------------------------------------------- 元数据解析

func TestOpenMMDBMeta(t *testing.T) {
	m, err := openMMDB(buildTestMMDB())
	if err != nil {
		t.Fatalf("openMMDB 失败: %v", err)
	}
	if m.meta.nodeCount != 2 {
		t.Errorf("nodeCount = %d, 期望 2", m.meta.nodeCount)
	}
	if m.meta.recordSize != 24 {
		t.Errorf("recordSize = %d, 期望 24", m.meta.recordSize)
	}
	if m.meta.databaseType != "Test-DB" {
		t.Errorf("databaseType = %q, 期望 Test-DB", m.meta.databaseType)
	}
	if m.meta.dataStart <= m.meta.treeStart {
		t.Errorf("dataStart(%d) 应大于 treeStart(%d)", m.meta.dataStart, m.meta.treeStart)
	}
}

// ---------------------------------------------------------------- 查询

func TestSearchMMDB(t *testing.T) {
	raw := buildTestMMDB()

	cases := []struct {
		ip      string
		wantISO string
	}{
		{"8.8.8.8", "US"},   // 00001000 前两位 00 -> node1.left  -> US
		{"64.1.1.1", "JP"},  // 01000000 前两位 01 -> node1.right -> JP
		{"200.1.1.1", "US"}, // 11001000 首位 1   -> node0.right -> US
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip).To4()
		rec, ok := searchMMDB(raw, ip)
		if !ok {
			t.Errorf("%s 应命中记录", c.ip)
			continue
		}
		country, _ := rec["country"].(map[string]any)
		if country == nil {
			t.Errorf("%s 记录缺少 country", c.ip)
			continue
		}
		if iso := flatStr(country["iso_code"]); iso != c.wantISO {
			t.Errorf("%s: iso_code = %q, 期望 %q", c.ip, iso, c.wantISO)
		}
	}
}

// ---------------------------------------------------------------- 字段抽取

func TestExtractCity(t *testing.T) {
	raw := buildTestMMDB()
	ip := net.ParseIP("8.8.8.8").To4()
	rec, ok := searchMMDB(raw, ip)
	if !ok {
		t.Fatal("应命中 US 记录")
	}

	info := &model.GeoIPInfo{}
	extractCity(rec, info)

	if info.CountryCode != "US" {
		t.Errorf("CountryCode = %q, 期望 US", info.CountryCode)
	}
	if info.CountryName != "United States" {
		t.Errorf("CountryName = %q, 期望 United States", info.CountryName)
	}
	if info.City != "Ashburn" {
		t.Errorf("City = %q, 期望 Ashburn", info.City)
	}
}

func TestExtractASN(t *testing.T) {
	cases := []struct {
		rec  map[string]any
		want string
	}{
		{
			map[string]any{
				"autonomous_system_number":       uint32(15169),
				"autonomous_system_organization": "Google LLC",
			},
			"AS15169 Google LLC",
		},
		{
			map[string]any{"autonomous_system_organization": "Cloudflare"},
			"Cloudflare",
		},
		{
			map[string]any{"autonomous_system_number": uint16(64512)},
			"AS64512",
		},
	}
	for i, c := range cases {
		info := &model.GeoIPInfo{}
		extractASN(c.rec, info)
		if info.AsOrganization != c.want {
			t.Errorf("case %d: AsOrganization = %q, 期望 %q", i, info.AsOrganization, c.want)
		}
	}
}

// ---------------------------------------------------------------- Lookup

// 内网地址不应触发查询。
func TestLookupSkipsPrivateIPs(t *testing.T) {
	l := New("local", "", "", nil, testLogger(t))
	for _, ip := range []string{"192.168.1.1", "10.0.0.1", "127.0.0.1", "", "not-an-ip"} {
		if v := l.IP(ip); v != nil {
			t.Errorf("IP(%q) 应返回 nil，得到 %+v", ip, v)
		}
	}
}

// local provider 但无 MMDB 时，公网 IP 也应返回 nil（不崩溃）。
func TestLookupLocalWithoutMMDB(t *testing.T) {
	l := New("local", "", "", nil, testLogger(t))
	if v := l.IP("8.8.8.8"); v != nil {
		t.Errorf("无 MMDB 时公网 IP 应返回 nil，得到 %+v", v)
	}
}

// 真实文件加载路径。
func TestLoadMMDBFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "GeoLite2-City.mmdb")
	if err := os.WriteFile(path, buildTestMMDB(), 0o644); err != nil {
		t.Fatal(err)
	}

	l := New("local", "", dir, nil, testLogger(t))
	if len(l.cityRaw) == 0 {
		t.Fatal("应从磁盘加载 City MMDB")
	}

	info := l.IP("8.8.8.8")
	if info == nil {
		t.Fatal("加载后应能查到 8.8.8.8")
	}
	if info.CountryCode != "US" {
		t.Errorf("CountryCode = %q, 期望 US", info.CountryCode)
	}

	// 内存缓存第二次应直接命中
	if l.IP("8.8.8.8") == nil {
		t.Error("缓存命中应仍然返回结果")
	}
}

// 坏文件不应 panic，只应记录并跳过。
func TestLoadMMDBCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "GeoLite2-City.mmdb")
	if err := os.WriteFile(path, []byte("garbage not mmdb"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := New("local", "", dir, nil, testLogger(t))
	if len(l.cityRaw) != 0 {
		t.Error("魔数不匹配的文件不应被加载")
	}
	if l.IP("8.8.8.8") != nil {
		t.Error("坏文件下查询应返回 nil")
	}
}

func TestLookupStats(t *testing.T) {
	l := New("online", "https://example.test/ip", "/tmp", nil, testLogger(t))
	st := l.Stats()
	if st["provider"] != "online" {
		t.Errorf("provider = %v", st["provider"])
	}
	if st["apiUrl"] != "https://example.test/ip" {
		t.Errorf("apiUrl = %v", st["apiUrl"])
	}
}

// 非法 provider 应回落为 online。
func TestNewInvalidProvider(t *testing.T) {
	l := New("bogus", "", "", nil, testLogger(t))
	if l.Provider() != "online" {
		t.Errorf("非法 provider 应回落为 online，得到 %q", l.Provider())
	}
}

func TestMeasureDataASize(t *testing.T) {
	raw := buildTestMMDB()
	m, _ := openMMDB(raw)

	off := m.meta.dataStart
	v, next, ok := m.decodeValue(off)
	t.Logf("dataA @%d decode ok=%v type=%T next=%d 占用=%d 字节",
		off, ok, v, next, next-off)

	// dataB 紧接其后
	v2, next2, ok2 := m.decodeValue(next)
	t.Logf("dataB @%d decode ok=%v type=%T next=%d", next, ok2, v2, next2)

	if mp, isMap := v2.(map[string]any); isMap {
		for k := range mp {
			t.Logf("  dataB key: %s", k)
		}
	}
}
