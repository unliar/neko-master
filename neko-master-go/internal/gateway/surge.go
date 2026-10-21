// Surge HTTP API 客户端与解析逻辑。
//
// 上游依据：
//   - apps/collector/src/modules/collector/surge.collector.ts（采集轮询）
//   - packages/shared/src/index.ts 的 SurgeRequest / SurgePoliciesData / SurgeRulesData
//   - API-DETAILS.md §2.1.1 / §2.1.2 / §2.1.4（/api/gateway/* 的 Surge 分支）
//
// 与 Clash 的关键差异：
//   - 鉴权头是 x-key，不是 Authorization: Bearer
//   - 采集入口是 GET /v1/requests/recent，不是 /connections 全量快照
//   - remoteAddress 是代理节点 IP，不是真实落地 IP（Surge 的 DNS 在代理侧完成）
//   - 策略链从 notes 的 "Policy decision path: rule -> g1 -> g2 -> final" 解析，
//     且需反转成 Clash 形态（chains[0]=出口，chains[last]=规则名）
package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// SurgeRequest 对齐 shared 的 SurgeRequest。
//
// 注意字段语义：inBytes/outBytes 是「下载/上传」字节累计值
// （in = 入站 = 下载，out = 出站 = 上传），与 Clash 的 upload/download 相反。
type SurgeRequest struct {
	ID              string   `json:"id"`
	Time            float64  `json:"time"`
	Timestamp       string   `json:"timestamp"`
	PolicyName      string   `json:"policyName"`
	OrigPolicyName  string   `json:"originalPolicyName"`
	Rule            string   `json:"rule"`
	ProcessPath     string   `json:"processPath"`
	RemoteHost      string   `json:"remoteHost"`
	RemoteAddress   string   `json:"remoteAddress"`
	RemotePort      *int     `json:"remotePort"`
	LocalAddress    string   `json:"localAddress"`
	LocalPort       *int     `json:"localPort"`
	SourceAddress   string   `json:"sourceAddress"`
	SourcePort      *int     `json:"sourcePort"`
	InBytes         float64  `json:"inBytes"`
	OutBytes        float64  `json:"outBytes"`
	InCurrentSpeed  *float64 `json:"inCurrentSpeed"`
	OutCurrentSpeed *float64 `json:"outCurrentSpeed"`
	Status          string   `json:"status"`
	Completed       *bool    `json:"completed"`
	Disconnected    *bool    `json:"disconnected"`
	Failed          *bool    `json:"failed"`
	Rejected        *bool    `json:"rejected"`
	StartDate       *float64 `json:"startDate"`
	CompletedDate   *float64 `json:"completedDate"`
	SetupCompletedD *float64 `json:"setupCompletedDate"`
	URL             string   `json:"URL"`
	Method          string   `json:"method"`
	Notes           []string `json:"notes"`
}

// SurgeRequestsData 是 /v1/requests/recent 的响应。
type SurgeRequestsData struct {
	Requests []SurgeRequest `json:"requests"`
}

// SurgePolicies 对齐 SurgePoliciesData。
type SurgePolicies struct {
	Proxies      []string `json:"proxies"`
	PolicyGroups []string `json:"policy-groups"`
	RawProxies   []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"-"`
	RawGroups []struct {
		Name     string   `json:"name"`
		Type     string   `json:"type"`
		Policy   string   `json:"policy"`
		Policies []string `json:"policies"`
	} `json:"-"`
}

// SurgeRuleItem 对齐 SurgeRuleItem。
type SurgeRuleItem struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
	Policy  string `json:"policy"`
	Raw     string `json:"raw"`
}

// SurgeRulesData 对齐 SurgeRulesData。
type SurgeRulesData struct {
	Rules             []SurgeRuleItem `json:"rules"`
	AvailablePolicies []string        `json:"availablePolicies"`
}

// SurgeClient 是对 Surge HTTP API 的客户端。
//
// 与 Client 分开是因为鉴权头与端点形态都不同。
type SurgeClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewSurgeClient 创建客户端。
func NewSurgeClient(baseURL, token string, insecure bool) *SurgeClient {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	tr := &http.Transport{
		Proxy:                 nil,
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &SurgeClient{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Transport: tr, Timeout: 15 * time.Second},
	}
}

// BaseURL 返回规范化地址。
func (c *SurgeClient) BaseURL() string { return c.baseURL }

// get 执行一次 GET。Surge 用 x-key 头鉴权。
func (c *SurgeClient) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("x-key", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return body, resp.StatusCode, err
}

// Version 探测 /v1/version（Surge 的 version 端点在 /v1 下）。
func (c *SurgeClient) Version(ctx context.Context) (string, error) {
	b, code, err := c.get(ctx, "/v1/version")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("surge /v1/version: HTTP %d", code)
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// RequestsRecent 拉取 /v1/requests/recent。
//
// 上游兼容两种调用形态：baseURL 已带 /v1/requests/recent 后缀时直接请求，
// 否则补全。这里统一补全。
func (c *SurgeClient) RequestsRecent(ctx context.Context) (*SurgeRequestsData, error) {
	b, code, err := c.get(ctx, "/v1/requests/recent")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("surge /v1/requests/recent: HTTP %d", code)
	}
	var d SurgeRequestsData
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("decode requests/recent: %w", err)
	}
	return &d, nil
}

// Policies 拉取 /v1/policies。
func (c *SurgeClient) Policies(ctx context.Context) (*SurgePolicies, error) {
	b, code, err := c.get(ctx, "/v1/policies")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("surge /v1/policies: HTTP %d", code)
	}
	var p SurgePolicies
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("decode policies: %w", err)
	}
	return &p, nil
}

// PolicyDetail 是 /v1/policies/{name} 的返回。
type PolicyDetail struct {
	Policy   string   `json:"policy"`
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Policies []string `json:"policies"`
}

// PolicyByName 拉取 /v1/policies/{groupName}，取 detail.policy 作为当前选中项。
//
// 依据 API-DETAILS §2.1.1：/api/gateway/proxies 用这个端点。
func (c *SurgeClient) PolicyByName(ctx context.Context, name string) (*PolicyDetail, error) {
	b, code, err := c.get(ctx, "/v1/policies/"+url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("surge /v1/policies/%s: HTTP %d", name, code)
	}
	var d PolicyDetail
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("decode policy detail: %w", err)
	}
	return &d, nil
}

// PolicyGroupSelect 是 /v1/policy_groups/select 的返回。
type PolicyGroupSelect struct {
	Policy string `json:"policy"`
	Type   string `json:"type"`
}

// SelectPolicyGroup 拉取 /v1/policy_groups/select?group_name=。
//
// 依据 API-DETAILS §2.1.2 与风险点 27：
// /api/gateway/providers/proxies 用这个端点，与 §2.1.1 不同，
// 两条路由不能合并。
func (c *SurgeClient) SelectPolicyGroup(ctx context.Context, group string) (*PolicyGroupSelect, error) {
	q := url.Values{"group_name": {group}}
	b, code, err := c.get(ctx, "/v1/policy_groups/select?"+q.Encode())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("surge select %s: HTTP %d", group, code)
	}
	var d PolicyGroupSelect
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("decode select: %w", err)
	}
	return &d, nil
}

// Rules 拉取 /v1/rules 并解析成结构化规则。
//
// Surge 的规则是字符串数组（"TYPE,PAYLOAD,POLICY"），
// 上游用 parseSurgeRule 还原成对象。见 gateway-utils.ts 的映射表。
func (c *SurgeClient) Rules(ctx context.Context) (*SurgeRulesData, error) {
	b, code, err := c.get(ctx, "/v1/rules")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("surge /v1/rules: HTTP %d", code)
	}
	var raw struct {
		Rules []string `json:"rules"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("decode rules: %w", err)
	}

	out := &SurgeRulesData{Rules: make([]SurgeRuleItem, 0, len(raw.Rules))}
	seen := map[string]struct{}{}
	for _, line := range raw.Rules {
		item, ok := ParseSurgeRule(line)
		if !ok {
			continue
		}
		out.Rules = append(out.Rules, *item)
		if _, ok := seen[item.Policy]; !ok {
			seen[item.Policy] = struct{}{}
			out.AvailablePolicies = append(out.AvailablePolicies, item.Policy)
		}
	}
	return out, nil
}

// ParseSurgeRule 对齐 parseSurgeRule。
//
// 映射表（源自 gateway-utils.ts）：
//
//	空行 / # 开头            → null
//	按 , 分割不足 2 段        → null
//	FINAL,<policy>           → {type:MATCH, payload:*, policy:unquote(p)}
//	TYPE,payload,policy(≥3) → {type, payload:parts[1], policy:unquote(parts[2])}
//	兜底 TYPE,policy          → {type, payload:'', policy:unquote(parts[1])||DIRECT}
//
// unquote(s) = 去掉首尾单/双引号。
func ParseSurgeRule(raw string) (*SurgeRuleItem, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil, false
	}
	parts := strings.Split(trimmed, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 2 {
		return nil, false
	}

	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 {
			if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
				s = s[1 : len(s)-1]
			}
		}
		return s
	}

	// FINAL → MATCH/*，这是类型改写（风险点 26）
	if strings.EqualFold(parts[0], "FINAL") {
		pol := unquote(parts[1])
		if pol == "" {
			pol = "DIRECT"
		}
		return &SurgeRuleItem{Type: "MATCH", Payload: "*", Policy: pol, Raw: trimmed}, true
	}

	if len(parts) >= 3 {
		return &SurgeRuleItem{
			Type:    parts[0],
			Payload: parts[1],
			Policy:  unquote(parts[2]),
			Raw:     trimmed,
		}, true
	}

	pol := unquote(parts[1])
	if pol == "" {
		pol = "DIRECT"
	}
	return &SurgeRuleItem{Type: parts[0], Payload: "", Policy: pol, Raw: trimmed}, true
}

var policyPathRe = regexp.MustCompile(`\[Rule\] Policy decision path: (.+)`)

// ExtractPolicyPath 从 notes 中提取策略决策路径。
//
// 上游格式："[Rule] Policy decision path: rule -> group1 -> group2 -> finalProxy"
// 返回 [finalProxy, ..., group2, group1, rule]（反转以对齐 Clash 的 chains 形态）。
func ExtractPolicyPath(notes []string) []string {
	if len(notes) == 0 {
		return nil
	}
	for _, note := range notes {
		m := policyPathRe.FindStringSubmatch(note)
		if m == nil {
			continue
		}
		segs := strings.Split(m[1], " -> ")
		path := make([]string, 0, len(segs))
		for _, s := range segs {
			s = strings.TrimSpace(s)
			if s != "" {
				path = append(path, s)
			}
		}
		if len(path) >= 2 {
			// 反转
			for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
				path[i], path[j] = path[j], path[i]
			}
			return path
		}
	}
	return nil
}

// ConvertSurgeChains 把 Surge 请求转成 Clash 形态的链路数组。
//
// 优先用 notes 里的完整路径；否则退化到 policyName/originalPolicyName。
// 两者都为空时返回 ["DIRECT"]。
func ConvertSurgeChains(policyName, origPolicyName string, notes []string) []string {
	if p := ExtractPolicyPath(notes); len(p) >= 2 {
		return p
	}
	chains := make([]string, 0, 2)
	if policyName != "" {
		chains = append(chains, policyName)
	}
	if origPolicyName != "" && origPolicyName != policyName {
		chains = append(chains, origPolicyName)
	}
	if len(chains) == 0 {
		return []string{"DIRECT"}
	}
	return chains
}

// SurgeSourceIP 提取请求的发起设备 IP。
//
// 上游逻辑：sourceAddress 是真正发请求的客户端；但 Surge 主机自己发的请求
// 会报回环地址，此时回落到 localAddress 以继续归属到主机 LAN IP。
func SurgeSourceIP(sourceAddress, localAddress string) string {
	src := extractHost(strings.TrimSpace(sourceAddress))
	if src != "" && !isLoopbackHost(src) {
		return src
	}
	return extractHost(strings.TrimSpace(localAddress))
}

func isLoopbackHost(h string) bool {
	h = strings.ToLower(h)
	if h == "::1" || h == "localhost" {
		return true
	}
	return strings.HasPrefix(h, "127.") || strings.HasPrefix(h, "::ffff:127.")
}

// extractHost 从 host:port 中取 host，兼容 IPv6 的 [addr]:port 形态。
func extractHost(hostWithPort string) string {
	if hostWithPort == "" {
		return ""
	}
	if strings.HasPrefix(hostWithPort, "[") {
		if i := strings.IndexByte(hostWithPort, ']'); i > 0 {
			return hostWithPort[1:i]
		}
		return hostWithPort
	}
	// IPv6 裸地址（多个冒号且无点）→ 整体返回
	if strings.Count(hostWithPort, ":") > 1 && !strings.Contains(hostWithPort, ".") {
		return hostWithPort
	}
	if i := strings.LastIndexByte(hostWithPort, ':'); i > 0 {
		return hostWithPort[:i]
	}
	return hostWithPort
}

func isIPAddr(s string) bool {
	if s == "" {
		return false
	}
	if strings.Contains(s, ".") {
		parts := strings.Split(s, ".")
		if len(parts) != 4 {
			return false
		}
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 255 {
				return false
			}
		}
		return true
	}
	// IPv6 粗判：含冒号且是合法 hex/冒号组合
	return strings.Contains(s, ":")
}

// isDomain 判断是否为域名（含点且不是 IP）。
func isDomain(s string) bool {
	if s == "" || !strings.Contains(s, ".") {
		return false
	}
	return !isIPAddr(s)
}

// ResolveTarget 从 Surge 请求提取 (domain, ip)。
//
// 关键：Surge 的 DNS 解析发生在代理服务器侧，因此
//   - remoteHost      = 目标域名
//   - remoteAddress   = 代理节点 IP（不是真实落地 IP）
//
// 所以 ip 只在 remoteHost 本身就是 IP（直连）时用 remoteHost，
// 否则只能取代理节点 IP。这与上游注释完全一致。
func ResolveTarget(req SurgeRequest) (domain, ip string) {
	remoteHost := strings.TrimSpace(req.RemoteHost)
	// remoteAddress 形如 "1.2.3.4 (Proxy)" 或 "1.2.3.4"
	remoteAddr := ""
	if req.RemoteAddress != "" {
		fields := strings.Fields(req.RemoteAddress)
		if len(fields) > 0 {
			remoteAddr = strings.TrimSpace(fields[0])
		}
	}
	host := extractHost(remoteHost)
	if isDomain(remoteHost) {
		domain = host
	}
	if isIPAddr(host) {
		ip = host
	} else if isIPAddr(remoteAddr) {
		ip = remoteAddr
	}
	return domain, ip
}

// RequestTime 返回请求的发生时刻（毫秒）。
func RequestTime(req SurgeRequest) time.Time {
	if req.Time > 0 {
		// Surge 的 time 是 Unix 秒（含小数）
		sec := int64(req.Time)
		nsec := int64((req.Time - float64(sec)) * 1e9)
		return time.Unix(sec, nsec).UTC()
	}
	if req.StartDate != nil && *req.StartDate > 0 {
		return time.Unix(int64(*req.StartDate), 0).UTC()
	}
	return time.Now().UTC()
}

// IsCompleted / IsDisconnected / IsFailed 便捷判断。
func IsCompleted(req SurgeRequest) bool { return req.Completed != nil && *req.Completed }
func IsDisconnected(req SurgeRequest) bool {
	return req.Disconnected != nil && *req.Disconnected
}
func IsFailed(req SurgeRequest) bool { return req.Failed != nil && *req.Failed }

// RuleName 返回规则名，缺失时退化到 "Match"（与 Clash 路径一致）。
func RuleName(req SurgeRequest) string {
	if strings.TrimSpace(req.Rule) != "" {
		return req.Rule
	}
	return "Match"
}
