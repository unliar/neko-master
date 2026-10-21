package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/collector"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// registerAgent 注册 agent 上报路由（反向推送模式）。
//
// Agent 部署在路由器上，主动 POST 数据到 collector，
// 而不是 collector 去连网关的 WebSocket。
func (s *Server) registerAgent() {
	s.mux.HandleFunc("POST /api/agent/heartbeat", s.handleAgentHeartbeat)
	s.mux.HandleFunc("POST /api/agent/report", s.handleAgentReport)
	s.mux.HandleFunc("POST /api/agent/config", s.handleAgentConfig)
	s.mux.HandleFunc("POST /api/agent/policy-state", s.handleAgentPolicyState)
}

// agentAuth 校验 agent token。
//
// 顺序（对齐上游 isAgentBackendAuthorized）：
//  1. backend 不存在 → 404
//  2. url 不是 agent:// → 400
//  3. token 未配置 → 403
//  4. 取 Bearer 或 x-agent-token，恒定时间比对 → 401
func (s *Server) agentAuth(w http.ResponseWriter, r *http.Request, backendID int64) bool {
	be, err := s.stats.GetBackend(backendID)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Backend not found")
		return false
	}
	if !strings.HasPrefix(strings.ToLower(be.URL), "agent://") {
		s.writeErr(w, http.StatusBadRequest, "Backend is not in agent mode (url must start with agent://)")
		return false
	}
	if be.Token == "" {
		s.writeErr(w, http.StatusForbidden, "Agent backend token is not configured")
		return false
	}

	provided := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		provided = strings.TrimSpace(h[7:])
	}
	if provided == "" {
		provided = strings.TrimSpace(r.Header.Get("x-agent-token"))
	}

	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(be.Token)) != 1 {
		s.writeErr(w, http.StatusUnauthorized, "Invalid agent token")
		return false
	}
	return true
}

// agentBackendID 从 body 或 query 里取 backendId。
func (s *Server) agentBackendID(r *http.Request, body map[string]any) int64 {
	if raw, ok := body["backendId"]; ok {
		switch v := raw.(type) {
		case float64:
			return int64(v)
		case string:
			var n int64
			for _, c := range v {
				if c < '0' || c > '9' {
					return 0
				}
				n = n*10 + int64(c-'0')
			}
			return n
		}
	}
	raw := r.URL.Query().Get("backendId")
	var n int64
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// ---------------------------------------------------------------- heartbeat

func (s *Server) handleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	id := s.agentBackendID(r, body)
	if id == 0 {
		s.writeErr(w, http.StatusBadRequest, "backendId is required")
		return
	}
	if !s.agentAuth(w, r, id) {
		return
	}

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	str := func(k string) string {
		if v, ok := body[k].(string); ok {
			return v
		}
		return ""
	}
	num := func(k string) sql.NullInt64 {
		if v, ok := body[k].(float64); ok {
			return sql.NullInt64{Int64: int64(v), Valid: true}
		}
		return sql.NullInt64{}
	}

	_, err := s.db.SQL().Exec(`
		INSERT INTO agent_heartbeats
			(backend_id, agent_id, hostname, version, gateway_type, gateway_url,
			 remote_ip, gateway_latency_ms, server_latency_ms, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(backend_id) DO UPDATE SET
			agent_id = excluded.agent_id,
			hostname = excluded.hostname,
			version = excluded.version,
			gateway_type = excluded.gateway_type,
			gateway_url = excluded.gateway_url,
			remote_ip = excluded.remote_ip,
			gateway_latency_ms = excluded.gateway_latency_ms,
			server_latency_ms = excluded.server_latency_ms,
			last_seen = excluded.last_seen,
			updated_at = CURRENT_TIMESTAMP`,
		id,
		firstNonEmpty(str("agentId"), "agent"),
		str("hostname"), str("version"), str("gatewayType"), str("gatewayUrl"),
		str("remoteIp"), num("gatewayLatencyMs"), num("serverLatencyMs"), now)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 每分钟记一条健康日志
	minute := time.Now().UTC().Format("2006-01-02T15:04")
	_, _ = s.db.SQL().Exec(`
		INSERT INTO backend_health_logs (backend_id, minute, status, latency_ms, server_latency_ms, message)
		VALUES (?, ?, 'healthy', ?, ?, NULL)
		ON CONFLICT(backend_id, minute) DO UPDATE SET
			status = 'healthy',
			latency_ms = excluded.latency_ms,
			server_latency_ms = excluded.server_latency_ms`,
		id, minute, num("gatewayLatencyMs"), num("serverLatencyMs"))

	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Heartbeat received",
	})
}

// ---------------------------------------------------------------- report

func (s *Server) handleAgentReport(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	id := s.agentBackendID(r, body)
	if id == 0 {
		s.writeErr(w, http.StatusBadRequest, "backendId is required")
		return
	}
	if !s.agentAuth(w, r, id) {
		return
	}

	// connections 由 agent 拉取上游 /connections 后原样转发
	raw, _ := body["connections"]
	conns, _ := raw.([]any)
	if len(conns) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": true, "accepted": 0})
		return
	}

	// timestampMs：agent 观测时刻，缺省用当前
	tsMs := time.Now().UTC()
	if v, ok := body["timestampMs"].(float64); ok && v > 0 {
		tsMs = time.UnixMilli(int64(v)).UTC()
	}

	resp := toConnectionsResponse(conns)
	tr := collector.NewTracker()
	updates := tr.Diff(resp, tsMs)

	// agent 模式没有本地 tracker 状态（每次都是新连接），
	// 因此这里直接按「本轮快照即增量」处理：agent 自己已做差。
	if rawDelta, ok := body["updates"].([]any); ok && len(rawDelta) > 0 {
		updates = parseAgentUpdates(rawDelta, tsMs)
	}

	if len(updates) == 0 {
		s.writeJSON(w, http.StatusOK, map[string]any{"success": true, "accepted": 0})
		return
	}

	writer := collector.NewWriterFor(s.db)
	if err := writer.Flush(id, updates); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"accepted":  len(updates),
		"timestamp": tsMs.Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------- config

func (s *Server) handleAgentConfig(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	id := s.agentBackendID(r, body)
	if id == 0 {
		s.writeErr(w, http.StatusBadRequest, "backendId is required")
		return
	}
	if !s.agentAuth(w, r, id) {
		return
	}

	// 返回 agent 需要的配置：采样间隔、网关地址等
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"config": map[string]any{
			"intervalMs":     s.cfg.FlushIntervalMS,
			"flushMaxItems":  s.cfg.FlushMaxBuffer,
			"realtimeMaxMin": s.cfg.RealtimeMaxMinute,
		},
	})
}

// ---------------------------------------------------------------- policy-state

func (s *Server) handleAgentPolicyState(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	id := s.agentBackendID(r, body)
	if id == 0 {
		s.writeErr(w, http.StatusBadRequest, "backendId is required")
		return
	}
	if !s.agentAuth(w, r, id) {
		return
	}

	// 落库到 agent_snapshots
	if raw, ok := body["policyState"]; ok {
		b, err := json.Marshal(raw)
		if err == nil {
			_, _ = s.db.SQL().Exec(`
				INSERT INTO agent_snapshots (backend_id, config_json, policy_state_json, updated_at)
				VALUES (?, '{}', ?, CURRENT_TIMESTAMP)
				ON CONFLICT(backend_id) DO UPDATE SET
					policy_state_json = excluded.policy_state_json,
					updated_at = CURRENT_TIMESTAMP`, id, string(b))
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ---------------------------------------------------------------- 工具

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// toConnectionsResponse 把 agent 转发的原始 JSON 转成模型。
func toConnectionsResponse(list []any) *model.ConnectionsResponse {
	out := &model.ConnectionsResponse{Connections: make([]model.Connection, 0, len(list))}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		var c model.Connection
		if v, ok := m["id"].(float64); ok {
			c.ID = jsonNum(v)
		} else if s, ok := m["id"].(string); ok {
			c.ID = s
		}
		if md, ok := m["metadata"].(map[string]any); ok {
			c.Metadata = model.ConnectionMetadata{
				Network:         mdStr(md, "network"),
				Type:            mdStr(md, "type"),
				SourceIP:        mdStr(md, "sourceIP"),
				DestinationIP:   mdStr(md, "destinationIP"),
				SourcePort:      mdStr(md, "sourcePort"),
				DestinationPort: mdStr(md, "destinationPort"),
				Host:            firstNonEmpty(mdStr(md, "host"), mdStr(md, "sniffHost")),
				Process:         mdStr(md, "process"),
			}
		}
		c.Upload = counterOf(m["upload"])
		c.Download = counterOf(m["download"])
		if chains, ok := m["chains"].([]any); ok {
			for _, ch := range chains {
				if s, ok := ch.(string); ok {
					c.Chains = append(c.Chains, s)
				}
			}
		}
		c.Rule = mdStr(m, "rule")
		c.RulePayload = mdStr(m, "rulePayload")
		c.Start = mdStr(m, "start")
		out.Connections = append(out.Connections, c)
	}
	return out
}

func jsonNum(f float64) string {
	// 去掉小数点后的 .0
	i := int64(f)
	return itoa(i)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func mdStr(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// counterOf 兼容 number / {total: n} / string。
func counterOf(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x < 0 {
			return 0
		}
		return int64(x)
	case string:
		var n int64
		ok := true
		for _, c := range x {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int64(c-'0')
		}
		if !ok {
			return 0
		}
		return n
	case map[string]any:
		return counterOf(x["total"])
	}
	return 0
}

// parseAgentUpdates 解析 agent 已做好的增量。
func parseAgentUpdates(list []any, ts time.Time) []model.TrafficUpdate {
	out := make([]model.TrafficUpdate, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, model.TrafficUpdate{
			TimestampMs: ts,
			Domain:      mdStr(m, "domain"),
			IP:          mdStr(m, "ip"),
			SourceIP:    mdStr(m, "sourceIP"),
			Chain:       firstNonEmpty(mdStr(m, "chain"), "DIRECT"),
			FullChain:   firstNonEmpty(mdStr(m, "fullChain"), mdStr(m, "chain"), "DIRECT"),
			Rule:        firstNonEmpty(mdStr(m, "rule"), "Match"),
			RulePayload: mdStr(m, "rulePayload"),
			Upload:      counterOf(m["upload"]),
			Download:    counterOf(m["download"]),
			Connections: counterOf(m["connections"]),
		})
	}
	return out
}

// r2ctx 从 http.Request 取 context。
func r2ctx(r *http.Request) context.Context { return r.Context() }
