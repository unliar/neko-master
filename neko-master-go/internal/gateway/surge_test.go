package gateway

import "testing"

func TestParseSurgeRuleFINAL(t *testing.T) {
	// FINAL → MATCH/*，这是类型改写（风险点 26）
	r, ok := ParseSurgeRule("FINAL,Proxy")
	if !ok {
		t.Fatal("应解析成功")
	}
	if r.Type != "MATCH" {
		t.Errorf("FINAL 应改写为 MATCH，得到 %q", r.Type)
	}
	if r.Payload != "*" {
		t.Errorf("payload 应为 *，得到 %q", r.Payload)
	}
	if r.Policy != "Proxy" {
		t.Errorf("policy = %q", r.Policy)
	}
}

func TestParseSurgeRuleEmptyAndComment(t *testing.T) {
	for _, in := range []string{"", "   ", "# comment", "#"} {
		if _, ok := ParseSurgeRule(in); ok {
			t.Errorf("%q 应返回 null", in)
		}
	}
}

func TestParseSurgeRuleTooFewParts(t *testing.T) {
	if _, ok := ParseSurgeRule("DOMAIN-SUFFIX"); ok {
		t.Error("只有 1 段应返回 null")
	}
}

func TestParseSurgeRuleThreeParts(t *testing.T) {
	r, ok := ParseSurgeRule("DOMAIN-SUFFIX,example.com,Proxy A")
	if !ok {
		t.Fatal("应解析成功")
	}
	if r.Type != "DOMAIN-SUFFIX" || r.Payload != "example.com" || r.Policy != "Proxy A" {
		t.Errorf("解析结果 = %+v", r)
	}
}

func TestParseSurgeRuleUnquote(t *testing.T) {
	r, _ := ParseSurgeRule(`GEOIP,CN,"My Proxy"`)
	if r.Policy != "My Proxy" {
		t.Errorf("引号应被剥除，得到 %q", r.Policy)
	}
	r2, _ := ParseSurgeRule("GEOIP,CN,'Single'")
	if r2.Policy != "Single" {
		t.Errorf("单引号应被剥除，得到 %q", r2.Policy)
	}
}

func TestParseSurgeRuleTwoPartFallback(t *testing.T) {
	r, ok := ParseSurgeRule("RULE-SET,MySet")
	if !ok {
		t.Fatal("应解析成功")
	}
	if r.Type != "RULE-SET" {
		t.Errorf("type = %q", r.Type)
	}
	if r.Payload != "" {
		t.Errorf("2 段形态 payload 应为空，得到 %q", r.Payload)
	}
	if r.Policy != "MySet" {
		t.Errorf("policy = %q", r.Policy)
	}
}

func TestExtractPolicyPathReverses(t *testing.T) {
	notes := []string{"[Rule] Policy decision path: Rule1 -> GroupA -> FinalProxy"}
	got := ExtractPolicyPath(notes)
	want := []string{"FinalProxy", "GroupA", "Rule1"}
	if len(got) != len(want) {
		t.Fatalf("长度 = %d，应为 %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %q，应为 %q（完整 %v）", i, got[i], want[i], got)
		}
	}
}

func TestExtractPolicyPathIgnoresShort(t *testing.T) {
	// 只有 1 段 → 不足 2 段，返回 nil
	if got := ExtractPolicyPath([]string{"[Rule] Policy decision path: OnlyOne"}); got != nil {
		t.Errorf("单段路径应被忽略，得到 %v", got)
	}
}

func TestExtractPolicyPathNoNotes(t *testing.T) {
	if got := ExtractPolicyPath(nil); got != nil {
		t.Error("无 notes 应返回 nil")
	}
	if got := ExtractPolicyPath([]string{"unrelated"}); got != nil {
		t.Error("无匹配应返回 nil")
	}
}

func TestConvertSurgeChainsFromNotes(t *testing.T) {
	chains := ConvertSurgeChains("Final", "Orig",
		[]string{"[Rule] Policy decision path: R -> G -> Final"})
	want := []string{"Final", "G", "R"}
	if len(chains) != 3 {
		t.Fatalf("长度 = %d，应为 3: %v", len(chains), chains)
	}
	for i := range want {
		if chains[i] != want[i] {
			t.Errorf("[%d] = %q，应为 %q", i, chains[i], want[i])
		}
	}
}

func TestConvertSurgeChainsFallback(t *testing.T) {
	chains := ConvertSurgeChains("Final", "Orig", nil)
	if len(chains) != 2 || chains[0] != "Final" || chains[1] != "Orig" {
		t.Errorf("fallback = %v", chains)
	}

	// policyName 与 original 相同 → 只保留一个
	c2 := ConvertSurgeChains("Same", "Same", nil)
	if len(c2) != 1 || c2[0] != "Same" {
		t.Errorf("去重后 = %v", c2)
	}

	// 都为空 → DIRECT
	c3 := ConvertSurgeChains("", "", nil)
	if len(c3) != 1 || c3[0] != "DIRECT" {
		t.Errorf("空值应为 [DIRECT]，得到 %v", c3)
	}
}

func TestSurgeSourceIP(t *testing.T) {
	// 正常客户端
	if got := SurgeSourceIP("192.168.1.50:12345", "192.168.1.1:9090"); got != "192.168.1.50" {
		t.Errorf("应取 sourceAddress，得到 %q", got)
	}
	// 回环源 → 回落 localAddress
	if got := SurgeSourceIP("127.0.0.1:12345", "192.168.1.7:9090"); got != "192.168.1.7" {
		t.Errorf("回环应回落 localAddress，得到 %q", got)
	}
	// ::1
	if got := SurgeSourceIP("[::1]:53", "10.0.0.2:9090"); got != "10.0.0.2" {
		t.Errorf("IPv6 回环应回落，得到 %q", got)
	}
	// 都没有 → 空
	if got := SurgeSourceIP("", ""); got != "" {
		t.Errorf("都空应为空，得到 %q", got)
	}
}

func TestExtractHost(t *testing.T) {
	cases := map[string]string{
		"192.168.1.5:8080":  "192.168.1.5",
		"192.168.1.5":       "192.168.1.5",
		"[2001:db8::1]:443": "2001:db8::1",
		"2001:db8::1":       "2001:db8::1",
		"":                  "",
	}
	for in, want := range cases {
		if got := extractHost(in); got != want {
			t.Errorf("extractHost(%q) = %q，应为 %q", in, got, want)
		}
	}
}

func TestResolveTargetDomain(t *testing.T) {
	// 走代理：remoteHost 是域名，remoteAddress 是代理节点 IP
	d, ip := ResolveTarget(SurgeRequest{
		RemoteHost:    "www.example.com",
		RemoteAddress: "1.2.3.4 (Proxy)",
	})
	if d != "www.example.com" {
		t.Errorf("domain = %q", d)
	}
	if ip != "1.2.3.4" {
		t.Errorf("ip 应为代理节点 IP，得到 %q", ip)
	}
}

func TestResolveTargetDirectIP(t *testing.T) {
	// 直连：remoteHost 本身是 IP
	d, ip := ResolveTarget(SurgeRequest{RemoteHost: "8.8.8.8", RemoteAddress: ""})
	if d != "" {
		t.Errorf("IP 不是域名，domain 应为空，得到 %q", d)
	}
	if ip != "8.8.8.8" {
		t.Errorf("ip = %q", ip)
	}
}

func TestResolveTargetStripsProxySuffix(t *testing.T) {
	_, ip := ResolveTarget(SurgeRequest{
		RemoteHost:    "a.com",
		RemoteAddress: "5.6.7.8 (Proxy)",
	})
	if ip != "5.6.7.8" {
		t.Errorf("应剥掉 (Proxy) 后缀，得到 %q", ip)
	}
}

func TestRequestTime(t *testing.T) {
	got := RequestTime(SurgeRequest{Time: 1761032400})
	if got.UTC().Year() != 2025 || got.UTC().Month() != 10 {
		t.Errorf("time 解析 = %v", got)
	}
	// startDate 兜底
	ts := 1761032400.0
	got2 := RequestTime(SurgeRequest{StartDate: &ts})
	if got2.UTC().Year() != 2025 {
		t.Errorf("startDate 兜底 = %v", got2)
	}
}

func TestRuleNameFallback(t *testing.T) {
	if got := RuleName(SurgeRequest{}); got != "Match" {
		t.Errorf("空 rule 应为 Match，得到 %q", got)
	}
	if got := RuleName(SurgeRequest{Rule: "DOMAIN-SUFFIX"}); got != "DOMAIN-SUFFIX" {
		t.Errorf("rule = %q", got)
	}
}
