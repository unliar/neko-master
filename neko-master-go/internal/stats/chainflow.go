package stats

import (
	"sort"
	"strings"
)

// ChainFlowNode 是图中的一个节点，对齐上游 rule.repository.ts 的节点形态。
//
// 关键：前端 api.ts 期望的是 name / layer / nodeType，
// 不是 Go 旧实现的 id / level / type。字段名与类型枚举都不同，
// 用错一个 ReactFlow 就渲染不出来。
type ChainFlowNode struct {
	Name             string   `json:"name"`
	Layer            int      `json:"layer"`
	NodeType         string   `json:"nodeType"` // rule | group | proxy
	TotalUpload      int64    `json:"totalUpload"`
	TotalDownload    int64    `json:"totalDownload"`
	TotalConnections int64    `json:"totalConnections"`
	Rules            []string `json:"rules"`
}

// ChainFlowLink 是图中的一条边，source/target 是节点下标（数字，不是名字）。
type ChainFlowLink struct {
	Source int      `json:"source"`
	Target int      `json:"target"`
	Rules  []string `json:"rules"`
}

// RulePath 是单条规则经过的节点与边下标。
type RulePath struct {
	NodeIndices []int `json:"nodeIndices"`
	LinkIndices []int `json:"linkIndices"`
}

// ChainFlowGraph 对齐前端 fetchJson 期望的完整结构。
type ChainFlowGraph struct {
	Nodes     []ChainFlowNode     `json:"nodes"`
	Links     []ChainFlowLink     `json:"links"`
	RulePaths map[string]RulePath `json:"rulePaths"`
	MaxLayer  int                 `json:"maxLayer"`
}

// flowRow 是 rule_chain_traffic 的一行。
type flowRow struct {
	Rule        string
	Chain       string
	TotalUpload int64
	TotalDown   int64
	TotalConns  int64
}

// RuleChainFlowAll 构建整张规则链路图。
//
// 逐段对齐上游 rule.repository.ts 的 getAllRuleChainFlows：
//
//  1. 取 rule_chain_traffic 前 maxRows 行（默认 2000，下限 100）
//  2. 每条 (rule, chain) 展开成 flowPath = [rule, ...chain]，
//     长度 < 2 的跳过
//  3. 累加节点流量、建立 link 集合、记录 rule → 节点/边
//  4. 判定节点类型：无入边 = rule 且 layer 强制 0；
//     无出边且非内置策略 = proxy；其余 = group
//  5. 所有 proxy 节点统一推到最右列（computedMaxLayer）
//  6. 稳定排序：layer → 类型序(rule<group<proxy) → 名称
//  7. links 的 source/target 换成排序后的节点下标
//  8. rulePaths 用下标表达
//
// maxRows 与上游 RULE_CHAIN_FLOW_MAX_ROWS 一致，默认 2000。
func (s *Service) RuleChainFlowAll(backendID int64, maxRows int) (*ChainFlowGraph, error) {
	if maxRows <= 0 {
		maxRows = 2000
	}
	if maxRows < 100 {
		maxRows = 100
	}

	rows, err := s.db.SQL().Query(`
		SELECT rule, chain, total_upload, total_download, total_connections
		FROM rule_chain_traffic
		WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC
		LIMIT ?`, backendID, maxRows)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	data := make([]flowRow, 0, maxRows)
	for rows.Next() {
		var r flowRow
		if err := rows.Scan(&r.Rule, &r.Chain, &r.TotalUpload, &r.TotalDown, &r.TotalConns); err != nil {
			continue
		}
		if r.Rule == "" || r.Chain == "" {
			continue
		}
		data = append(data, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return buildChainFlowGraph(data), nil
}

// nodeAcc 是构建过程中的节点累加器。
type nodeAcc struct {
	upload int64
	down   int64
	conns  int64
	rules  map[string]struct{}
	layer  int
}

func buildChainFlowGraph(rows []flowRow) *ChainFlowGraph {
	nodes := map[string]*nodeAcc{}
	linkRules := map[string]map[string]struct{}{} // "src\u0000tgt" -> rules
	pathNodes := map[string]map[string]struct{}{} // rule -> node names
	pathLinks := map[string]map[string]struct{}{} // rule -> link keys
	outgoing := map[string]map[string]struct{}{}
	incoming := map[string]map[string]struct{}{}

	acc := func(name string) *nodeAcc {
		n, ok := nodes[name]
		if !ok {
			n = &nodeAcc{rules: map[string]struct{}{}}
			nodes[name] = n
		}
		return n
	}
	addTo := func(m map[string]map[string]struct{}, k, v string) {
		s, ok := m[k]
		if !ok {
			s = map[string]struct{}{}
			m[k] = s
		}
		s[v] = struct{}{}
	}

	for _, row := range rows {
		if _, ok := pathNodes[row.Rule]; !ok {
			pathNodes[row.Rule] = map[string]struct{}{}
			pathLinks[row.Rule] = map[string]struct{}{}
		}

		path := buildRuleFlowPath(row.Rule, row.Chain)
		if len(path) < 2 {
			continue
		}

		for i, name := range path {
			n := acc(name)
			n.upload += row.TotalUpload
			n.down += row.TotalDown
			n.conns += row.TotalConns
			n.rules[row.Rule] = struct{}{}
			if i > n.layer {
				n.layer = i
			}
			pathNodes[row.Rule][name] = struct{}{}
		}

		for i := 0; i < len(path)-1; i++ {
			src, tgt := path[i], path[i+1]
			key := src + "\x00" + tgt
			if _, ok := linkRules[key]; !ok {
				linkRules[key] = map[string]struct{}{}
			}
			linkRules[key][row.Rule] = struct{}{}
			pathLinks[row.Rule][key] = struct{}{}
			addTo(outgoing, src, tgt)
			addTo(incoming, tgt, src)
		}
	}

	// ---- \u8282\u70b9\u7c7b\u578b\u5224\u5b9a ----
	nodeType := map[string]string{}
	maxLayer := 0
	for name, n := range nodes {
		hasOut := len(outgoing[name]) > 0
		hasIn := len(incoming[name]) > 0
		switch {
		case !hasIn:
			nodeType[name] = "rule"
			n.layer = 0
		case !hasOut && !isBuiltInPolicy(name):
			nodeType[name] = "proxy"
		default:
			nodeType[name] = "group"
		}
		if n.layer > maxLayer {
			maxLayer = n.layer
		}
	}
	// proxy \u7edf\u4e00\u63a8\u5230\u6700\u53f3\u5217
	for name, n := range nodes {
		if nodeType[name] == "proxy" {
			n.layer = maxLayer
		}
	}

	// ---- \u7a33\u5b9a\u6392\u5e8f ----
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	order := func(t string) int {
		switch t {
		case "rule":
			return 0
		case "group":
			return 1
		}
		return 2
	}
	sort.Slice(names, func(i, j int) bool {
		ni, nj := nodes[names[i]], nodes[names[j]]
		if ni.layer != nj.layer {
			return ni.layer < nj.layer
		}
		ti, tj := order(nodeType[names[i]]), order(nodeType[names[j]])
		if ti != tj {
			return ti < tj
		}
		return names[i] < names[j]
	})

	outNodes := make([]ChainFlowNode, 0, len(names))
	idxOf := map[string]int{}
	for i, name := range names {
		n := nodes[name]
		idxOf[name] = i
		rules := make([]string, 0, len(n.rules))
		for r := range n.rules {
			rules = append(rules, r)
		}
		sort.Strings(rules)
		outNodes = append(outNodes, ChainFlowNode{
			Name:             name,
			Layer:            n.layer,
			NodeType:         nodeType[name],
			TotalUpload:      n.upload,
			TotalDownload:    n.down,
			TotalConnections: n.conns,
			Rules:            rules,
		})
	}

	// ---- links\uff1a\u4e0b\u6807\u5f62\u5f0f ----
	keys := make([]string, 0, len(linkRules))
	for k := range linkRules {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		si, ti := splitLinkKey(keys[i])
		sj, tj := splitLinkKey(keys[j])
		if idxOf[si] != idxOf[sj] {
			return idxOf[si] < idxOf[sj]
		}
		return idxOf[ti] < idxOf[tj]
	})

	outLinks := make([]ChainFlowLink, 0, len(keys))
	for _, k := range keys {
		src, tgt := splitLinkKey(k)
		si, okS := idxOf[src]
		ti, okT := idxOf[tgt]
		if !okS || !okT {
			continue
		}
		rules := make([]string, 0, len(linkRules[k]))
		for r := range linkRules[k] {
			rules = append(rules, r)
		}
		sort.Strings(rules)
		outLinks = append(outLinks, ChainFlowLink{Source: si, Target: ti, Rules: rules})
	}

	// ---- rulePaths ----
	paths := map[string]RulePath{}
	for rule, nameSet := range pathNodes {
		ni := make([]int, 0, len(nameSet))
		for name := range nameSet {
			if idx, ok := idxOf[name]; ok {
				ni = append(ni, idx)
			}
		}
		sort.Ints(ni)

		lk := pathLinks[rule]
		li := []int{}
		for i, l := range outLinks {
			key := outNodes[l.Source].Name + "\x00" + outNodes[l.Target].Name
			if _, ok := lk[key]; ok {
				li = append(li, i)
			}
		}
		paths[rule] = RulePath{NodeIndices: ni, LinkIndices: li}
	}

	// ---- maxLayer \u4e0a\u6e38\u7528\u8282\u70b9\u7684 layer\uff0c\u8fd9\u91cc\u7528\u56fa\u5b9a\u7ed3\u6784\u4e0a\u7684\u503c\uff08\u542b proxy \u63a8\u53f3\u540e\uff09----
	finalMax := 0
	for _, n := range outNodes {
		if n.Layer > finalMax {
			finalMax = n.Layer
		}
	}

	return &ChainFlowGraph{
		Nodes:     outNodes,
		Links:     outLinks,
		RulePaths: paths,
		MaxLayer:  finalMax,
	}
}

// buildRuleFlowPath \u5c06 (rule, chain) \u5c55\u5f00\u4e3a\u6d41\u5411\u8def\u5f84\u3002
//
// \u5bf9\u9f50\u4e0a\u6e38 buildRuleFlowPathWithConfig \u7684\u65e0 proxyConfig \u5206\u652f\uff08\u56e0\u4e3a\u6211\u4eec\u6ca1\u6709\u4ee3\u7406\u7ec4\u914d\u7f6e\u53ef\u7528\uff09\uff1a
//   - \u5b58\u50a8\u683c\u5f0f\u662f "proxy > ... > rule"\uff08\u4e0a\u6e38 Tracker \u7528 joinChain \u62fc\u7684\u5b8c\u6574\u94fe\uff09
//   - \u56e0\u6b64\u9700\u8981\u53cd\u8f6c\u6210 "rule > ... > proxy"
//   - \u82e5 rule \u672c\u8eab\u5c31\u5728 chain \u91cc\uff0c\u622a\u5230\u5b83\u4e3a\u6b62\u518d\u53cd\u8f6c
//   - \u5426\u5219\u524d\u7f6e rule
func buildRuleFlowPath(rule, chain string) []string {
	parts := splitChainParts(chain)
	if len(parts) == 0 {
		return []string{rule}
	}

	// 对齐上游 buildRuleFlowPathWithConfig（无 proxyConfig 分支）：
	//   const ruleIndex = findRuleIndexInChain(enrichedParts, rule);
	//   return enrichedParts.slice(0, ruleIndex + 1).reverse();
	//
	// 关键：先在**原始顺序**里截到 rule 为止，再整段反转。
	// 存储顺序是 proxy → ... → rule，因此 slice(0, idx+1) 拿到的是
	// [proxy, ..., rule]，reverse() 后成 [rule, ..., proxy] —— 即流向。
	//
	// 之前我写成“先整体反转再截取”，导致 rule 总映射到下标 0，截出来只剩
	// [rule] 一个节点，整张图就空了。顺序不能换。
	if idx := indexOfRule(parts, rule); idx >= 0 {
		seg := parts[:idx+1]
		rev := make([]string, len(seg))
		for i, p := range seg {
			rev[len(seg)-1-i] = p
		}
		return rev
	}

	// rule 不在 chain 里（如 minute_dim 行）：整体反转并前置 rule。
	rev := make([]string, len(parts))
	for i, p := range parts {
		rev[len(parts)-1-i] = p
	}
	return append([]string{rule}, rev...)
}

func splitChainParts(chain string) []string {
	raw := strings.Split(chain, ">")
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func indexOfRule(parts []string, rule string) int {
	for i, p := range parts {
		if p == rule {
			return i
		}
	}
	nr := normalizeFlowLabel(rule)
	if nr == "" {
		return -1
	}
	for i, p := range parts {
		if normalizeFlowLabel(p) == nr {
			return i
		}
	}
	return -1
}

// normalizeFlowLabel \u5bf9\u9f50\u4e0a\u6e38\uff1a\u53bb\u5f00\u5934\u7b26\u53f7\u3001\u538b\u7f29\u7a7a\u767d\u3001\u5c0f\u5199\u3002
// \u4e0a\u6e38\u8fd8\u505a\u4e86 NFKC\uff0cGo \u65e0\u5bf9\u5e94\u5185\u7f6e\uff0c\u9010\u5b57\u8282\u505a Unicode \u5f52\u4e00\u5316\uff08golang.org/x/text \u672a\u5f15\u5165\uff0c\u6545\u7528\u7b80\u5316\u7248\uff09\u3002
func normalizeFlowLabel(label string) string {
	label = strings.TrimSpace(label)
	// \u53bb\u6389\u5f00\u5934\u7684\u975e\u5b57\u6bcd\u6570\u5b57\u5b57\u7b26
	for len(label) > 0 && !isAlnumRune(rune(label[0])) && label[0] < 0x80 {
		label = label[1:]
	}
	label = strings.Join(strings.Fields(label), " ")
	return strings.ToLower(label)
}

func isAlnumRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// isBuiltInPolicy \u5bf9\u9f50\u4e0a\u6e38\uff1a\u5185\u7f6e\u7b56\u7565\u4e0d\u5f53 proxy \u8282\u70b9\u3002
func isBuiltInPolicy(name string) bool {
	return name == "DIRECT" || name == "REJECT" || name == "REJECT-TINY"
}

func splitLinkKey(k string) (src, tgt string) {
	i := strings.IndexByte(k, 0)
	if i < 0 {
		return k, ""
	}
	return k[:i], k[i+1:]
}
