package api

import (
	"encoding/json"
	"testing"

	"github.com/unliar/neko-master/neko-master-go/internal/stats"
)

func intPtr(v int) *int       { return &v }
func strPtr(v string) *string { return &v }

// 验证 wsMessage 能正确解析 ipsPage / domainsPage。
// 这两个字段命名只差一个字母，json tag 极易写错。
func TestWSMessageParsePages(t *testing.T) {
	raw := `{
		"type":"subscribe","backendId":1,"minPushIntervalMs":1000,
		"includeSummary":true,
		"includeDomainsPage":true,"domainsPageLimit":5,
		"includeIPsPage":true,"ipsPageLimit":5
	}`

	var m wsMessage
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m.IncludeDomainsPage == nil || !*m.IncludeDomainsPage {
		t.Errorf("IncludeDomainsPage 未解析: %v", m.IncludeDomainsPage)
	}
	if m.IncludeIPsPage == nil || !*m.IncludeIPsPage {
		t.Errorf("IncludeIPsPage 未解析: %v", m.IncludeIPsPage)
	}
	if m.DomainsPageLimit == nil || *m.DomainsPageLimit != 5 {
		t.Errorf("DomainsPageLimit = %v", m.DomainsPageLimit)
	}
	if m.IPsPageLimit == nil || *m.IPsPageLimit != 5 {
		t.Errorf("IPsPageLimit = %v", m.IPsPageLimit)
	}
}

// 只给 limit 时，其余参数应补齐默认值。
func TestNormPageDefaults(t *testing.T) {
	p, ok := stats.NormIPPage(nil, intPtr(5), nil, nil, nil)
	if !ok {
		t.Fatal("归一化应成功")
	}
	if p.Limit != 5 {
		t.Errorf("Limit = %d, 期望 5", p.Limit)
	}
	if p.Offset != 0 {
		t.Errorf("Offset = %d, 期望 0", p.Offset)
	}
	if p.SortOrder != "desc" {
		t.Errorf("SortOrder = %q, 期望 desc", p.SortOrder)
	}
	if p.SortBy == "" {
		t.Error("SortBy 应有默认列")
	}
}

// limit 超上限应被截到 200（WS 侧约束，与 HTTP 的 5000 不同）。
func TestNormPageLimitClamp(t *testing.T) {
	p, ok := stats.NormIPPage(nil, intPtr(9999), nil, nil, nil)
	if !ok {
		t.Fatal("归一化应成功")
	}
	if p.Limit != 200 {
		t.Errorf("Limit = %d, 期望被截到 200", p.Limit)
	}
}

// sortBy 不在白名单 → 整个 page 设置被忽略（复刻上游行为）。
func TestNormPageBadSortBy(t *testing.T) {
	if _, ok := stats.NormIPPage(nil, intPtr(5), strPtr("bogus"), nil, nil); ok {
		t.Error("非法 sortBy 应返回 ok=false")
	}
}

// sortOrder 不是 asc/desc → 同样忽略。
func TestNormPageBadSortOrder(t *testing.T) {
	if _, ok := stats.NormIPPage(nil, intPtr(5), nil, strPtr("sideways"), nil); ok {
		t.Error("非法 sortOrder 应返回 ok=false")
	}
}

// 域名与 IP 的白名单不同：domain 有 domain 键，ip 有 ip 键，互不通用。
func TestNormPageSortKeyWhitelists(t *testing.T) {
	if _, ok := stats.NormDomainPage(nil, intPtr(5), strPtr("domain"), nil, nil); !ok {
		t.Error("domains 应接受 domain 排序键")
	}
	if _, ok := stats.NormIPPage(nil, intPtr(5), strPtr("domain"), nil, nil); ok {
		t.Error("ips 不应接受 domain 排序键")
	}
}
