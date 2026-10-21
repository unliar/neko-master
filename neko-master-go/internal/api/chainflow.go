package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
)

// handleConnectionsLegacy 对应 GET /api/stats/connections。
//
// 上游源码明确标注 @deprecated：connection_logs 已停写，
// 该接口恒返回空数组。保留路由只为兼容前端。
func (s *Server) handleConnectionsLegacy(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	s.writeJSON(w, http.StatusOK, []any{})
}

// ChainFlowNode 是链路图里的一个节点。
type ChainFlowNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Level int    `json:"level"`
	Type  string `json:"type"`
}

// ChainFlowLink 是链路图里的一条边。
type ChainFlowLink struct {
	Source    string `json:"source"`
	Target    string `json:"target"`
	Upload    int64  `json:"upload"`
	Download  int64  `json:"download"`
	Total     int64  `json:"total"`
	ConnCount int64  `json:"connections"`
}

// ChainFlowResponse 是 /api/stats/rules/chain-flow 的响应。
//
// 注意：上游 chain-flow 返回的是匿名类型（非 RuleChainFlowAll），
// 且 links 用数组下标做引用，节点顺序依赖 Map 插入顺序，不保证稳定。
type ChainFlowResponse struct {
	Nodes []ChainFlowNode `json:"nodes"`
	Links []ChainFlowLink `json:"links"`
}

// handleChainFlow 对应 GET /api/stats/rules/chain-flow。
func (s *Server) handleChainFlow(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	rule := r.URL.Query().Get("rule")
	if rule == "" {
		s.writeErr(w, http.StatusBadRequest, "Rule parameter is required")
		return
	}

	// 从 rule_chain_traffic 取该规则经过的链路
	rows, err := s.db.SQL().Query(`
		SELECT chain, total_upload, total_download, total_connections
		FROM rule_chain_traffic
		WHERE backend_id = ? AND rule = ?
		ORDER BY (total_upload + total_download) DESC`, id, rule)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	// 用稳定顺序收集节点：先按出现顺序编号
	nodeIdx := map[string]int{}
	nodes := []ChainFlowNode{}
	links := []ChainFlowLink{}
	linkSet := map[string]int{}

	for rows.Next() {
		var chain string
		var up, down, conns int64
		if err := rows.Scan(&chain, &up, &down, &conns); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		hops := strings.Split(chain, " > ")
		// 规则本身是第一个节点
		if _, ok := nodeIdx[rule]; !ok {
			nodeIdx[rule] = len(nodes)
			nodes = append(nodes, ChainFlowNode{ID: rule, Name: rule, Level: 0, Type: "rule"})
		}
		prev := rule
		for level, hop := range hops {
			hop = strings.TrimSpace(hop)
			if hop == "" {
				continue
			}
			if _, ok := nodeIdx[hop]; !ok {
				nodeIdx[hop] = len(nodes)
				nodes = append(nodes, ChainFlowNode{
					ID:    hop,
					Name:  hop,
					Level: level + 1,
					Type:  "proxy",
				})
			}
			key := prev + "→" + hop
			if li, ok := linkSet[key]; ok {
				links[li].Upload += up
				links[li].Download += down
				links[li].Total += up + down
				links[li].ConnCount += conns
			} else {
				linkSet[key] = len(links)
				links = append(links, ChainFlowLink{
					Source:    prev,
					Target:    hop,
					Upload:    up,
					Download:  down,
					Total:     up + down,
					ConnCount: conns,
				})
			}
			prev = hop
		}
	}

	s.writeJSON(w, http.StatusOK, ChainFlowResponse{Nodes: nodes, Links: links})
}

// handleChainFlowAll 对应 GET /api/stats/rules/chain-flow-all。
//
// 与 chain-flow 的区别：这是全量规则链路，响应结构也不同（RuleChainFlowAll）。
func (s *Server) handleChainFlowAll(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	limit := parseLimit(r.URL.Query().Get("limit"), 2000, 2000)

	rows, err := s.db.SQL().Query(`
		SELECT rule, chain, total_upload, total_download, total_connections
		FROM rule_chain_traffic
		WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC
		LIMIT ?`, id, limit)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	nodeIdx := map[string]int{}
	nodes := []ChainFlowNode{}
	links := []ChainFlowLink{}
	linkSet := map[string]int{}

	for rows.Next() {
		var rule, chain string
		var up, down, conns int64
		if err := rows.Scan(&rule, &chain, &up, &down, &conns); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		if _, ok := nodeIdx[rule]; !ok {
			nodeIdx[rule] = len(nodes)
			nodes = append(nodes, ChainFlowNode{ID: rule, Name: rule, Level: 0, Type: "rule"})
		}
		prev := rule
		for level, hop := range strings.Split(chain, " > ") {
			hop = strings.TrimSpace(hop)
			if hop == "" {
				continue
			}
			if _, ok := nodeIdx[hop]; !ok {
				nodeIdx[hop] = len(nodes)
				nodes = append(nodes, ChainFlowNode{ID: hop, Name: hop, Level: level + 1, Type: "proxy"})
			}
			key := prev + "→" + hop
			if li, ok := linkSet[key]; ok {
				links[li].Upload += up
				links[li].Download += down
				links[li].Total += up + down
				links[li].ConnCount += conns
			} else {
				linkSet[key] = len(links)
				links = append(links, ChainFlowLink{
					Source: prev, Target: hop,
					Upload: up, Download: down, Total: up + down, ConnCount: conns,
				})
			}
			prev = hop
		}
	}

	// RuleChainFlowAll 的形态：{ rules: [...], nodes, links }
	type ruleEntry struct {
		Rule      string `json:"rule"`
		Upload    int64  `json:"upload"`
		Download  int64  `json:"download"`
		Total     int64  `json:"total"`
		ConnCount int64  `json:"connections"`
	}
	ruleRows, _ := s.db.SQL().Query(`
		SELECT rule, total_upload, total_download, total_connections
		FROM rule_stats WHERE backend_id = ?
		ORDER BY (total_upload + total_download) DESC LIMIT 50`, id)
	rules := []ruleEntry{}
	for ruleRows.Next() {
		var e ruleEntry
		_ = ruleRows.Scan(&e.Rule, &e.Upload, &e.Download, &e.ConnCount)
		e.Total = e.Upload + e.Download
		rules = append(rules, e)
	}
	ruleRows.Close()

	s.writeJSON(w, http.StatusOK, map[string]any{
		"rules": rules,
		"nodes": nodes,
		"links": links,
	})
}

// handleRuleProxyMap 对应 GET /api/stats/rule-proxy-map。
//
// 注意：上游这里返回 {rule, proxies[]} 形态，与 shared 的 RuleProxyMapping
// 不是同一个结构 —— 必须按 {rule, proxies[]} 输出。
func (s *Server) handleRuleProxyMap(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}

	rows, err := s.db.SQL().Query(`
		SELECT rule, proxy FROM rule_proxy_map WHERE backend_id = ? ORDER BY rule, proxy`, id)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	order := []string{}
	byRule := map[string][]string{}
	for rows.Next() {
		var rule, proxy sql.NullString
		if err := rows.Scan(&rule, &proxy); err != nil {
			continue
		}
		rk := rule.String
		if _, ok := byRule[rk]; !ok {
			order = append(order, rk)
		}
		if proxy.Valid && proxy.String != "" {
			byRule[rk] = append(byRule[rk], proxy.String)
		}
	}

	type entry struct {
		Rule    string   `json:"rule"`
		Proxies []string `json:"proxies"`
	}
	out := []entry{}
	for _, rk := range order {
		ps := byRule[rk]
		if ps == nil {
			ps = []string{}
		}
		out = append(out, entry{Rule: rk, Proxies: ps})
	}
	s.writeJSON(w, http.StatusOK, out)
}

var _ = json.Marshal
