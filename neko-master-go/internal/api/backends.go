package api

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// registerBackends 注册 backend CRUD 路由。
//
// 依据前端 lib/api.ts 的调用清单：
//
//	GET    /api/backends                    列表
//	POST   /api/backends                    新增
//	PUT    /api/backends/{id}               修改
//	DELETE /api/backends/{id}               删除
//	POST   /api/backends/{id}/activate      设为活跃
//	POST   /api/backends/{id}/listening      开关监听
//	POST   /api/backends/{id}/clear-data     清数据
//	POST   /api/backends/{id}/test           连通性测试
//	POST   /api/backends/{id}/rotate-agent-token  轮换 agent token
//	POST   /api/backends/test                未保存时测试
//	GET    /api/backends/health/history      健康历史
func (s *Server) registerBackends() {
	s.mux.HandleFunc("POST /api/backends", s.handleBackendCreate)
	s.mux.HandleFunc("GET /api/backends/health/history", s.handleBackendHealthHistory)
	// 列表 / active / listening 由 router.go 注册（带与不带斜杠两种形式）

	s.mux.HandleFunc("PUT /api/backends/{id}", s.handleBackendUpdate)
	s.mux.HandleFunc("DELETE /api/backends/{id}", s.handleBackendDelete)
	s.mux.HandleFunc("POST /api/backends/{id}/activate", s.handleBackendActivate)
	s.mux.HandleFunc("POST /api/backends/{id}/listening", s.handleBackendSetListening)
	s.mux.HandleFunc("POST /api/backends/{id}/clear-data", s.handleBackendClearData)
	s.mux.HandleFunc("POST /api/backends/{id}/test", s.handleBackendTestByID)
	s.mux.HandleFunc("POST /api/backends/{id}/rotate-agent-token", s.handleRotateAgentToken)
	s.mux.HandleFunc("POST /api/backends/test", s.handleBackendTest)
}

// backendInput 是创建/更新 backend 的请求体。
type backendInput struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Token     string `json:"token"`
	Type      string `json:"type"`
	Enabled   *bool  `json:"enabled"`
	Listening *bool  `json:"listening"`
}

func (in backendInput) normalizeType() string {
	if strings.EqualFold(in.Type, "surge") {
		return "surge"
	}
	if strings.EqualFold(in.Type, "agent") {
		return "agent"
	}
	return "clash"
}

// ---------------------------------------------------------------- 列表

// handleBackendsList 覆盖 router.go 里的同名处理器，这里保留但改为调用完整实现。
func (s *Server) handleBackendsList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SQL().Query(`
		SELECT id, name, url, COALESCE(token,''), COALESCE(type,'clash'),
		       enabled, is_active, listening,
		       COALESCE(created_at,''), COALESCE(updated_at,'')
		FROM backend_configs ORDER BY id`)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	out := []model.Backend{}
	for rows.Next() {
		var b model.Backend
		var enabled, isActive, listening int
		var token string
		if err := rows.Scan(&b.ID, &b.Name, &b.URL, &token, &b.Type,
			&enabled, &isActive, &listening, &b.CreatedAt, &b.UpdatedAt); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		b.Enabled = enabled != 0
		b.IsActive = isActive != 0
		b.Listening = listening != 0
		b.HasToken = token != ""
		b.Token = maskToken(token)
		out = append(out, b)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleBackendActive 返回当前激活的 backend。
func (s *Server) handleBackendActive(w http.ResponseWriter, r *http.Request) {
	id, err := s.stats.ActiveBackendID()
	if err != nil {
		s.writeJSON(w, http.StatusOK, nil)
		return
	}
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeJSON(w, http.StatusOK, nil)
		return
	}
	be.HasToken = be.Token != ""
	be.Token = maskToken(be.Token)
	s.writeJSON(w, http.StatusOK, be)
}

// handleBackendListening 返回所有 listening 状态的 backend id。
func (s *Server) handleBackendListening(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.SQL().Query(`SELECT id FROM backend_configs WHERE listening = 1 ORDER BY id`)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	s.writeJSON(w, http.StatusOK, ids)
}

// ---------------------------------------------------------------- 新增

func (s *Server) handleBackendCreate(w http.ResponseWriter, r *http.Request) {
	var in backendInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.URL) == "" {
		s.writeErr(w, http.StatusBadRequest, "Name and url are required")
		return
	}

	typ := in.normalizeType()
	url := strings.TrimSpace(in.URL)

	// agent 模式：url 必须是 agent://，并自动生成 token
	if typ == "agent" && !strings.HasPrefix(strings.ToLower(url), "agent://") {
		s.writeErr(w, http.StatusBadRequest, "Agent backend url must start with agent://")
		return
	}

	// 名称唯一
	var dup int
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs WHERE name = ?`, in.Name).Scan(&dup)
	if dup > 0 {
		s.writeErr(w, http.StatusBadRequest, "Backend name already exists")
		return
	}

	token := strings.TrimSpace(in.Token)
	if typ == "agent" && token == "" {
		token = generateAgentToken()
	}

	enabled, listening := true, true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	if in.Listening != nil {
		listening = *in.Listening
	}

	res, err := s.db.SQL().Exec(`
		INSERT INTO backend_configs (name, url, token, type, enabled, is_active, listening)
		VALUES (?, ?, ?, ?, ?, 0, ?)`, in.Name, url, token, typ, enabled, listening)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()

	// 第一个 backend 自动设为活跃
	var cnt int
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs`).Scan(&cnt)
	if cnt == 1 {
		_, _ = s.db.SQL().Exec(`UPDATE backend_configs SET is_active = 1 WHERE id = ?`, id)
	}

	resp := map[string]any{
		"id":       id,
		"isActive": cnt == 1,
		"message":  "Backend created",
	}
	if typ == "agent" && in.Token == "" {
		resp["agentToken"] = token
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------- 修改

func (s *Server) handleBackendUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}
	var in backendInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	// 只更新提供的字段
	sets := []string{"updated_at = CURRENT_TIMESTAMP"}
	args := []any{}
	if in.Name != "" {
		sets = append(sets, "name = ?")
		args = append(args, in.Name)
	}
	if in.URL != "" {
		sets = append(sets, "url = ?")
		args = append(args, strings.TrimSpace(in.URL))
	}
	if in.Token != "" {
		sets = append(sets, "token = ?")
		args = append(args, strings.TrimSpace(in.Token))
	}
	if in.Type != "" {
		sets = append(sets, "type = ?")
		args = append(args, in.normalizeType())
	}
	if in.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolToInt(*in.Enabled))
	}
	if in.Listening != nil {
		sets = append(sets, "listening = ?")
		args = append(args, boolToInt(*in.Listening))
	}
	args = append(args, id)

	_, err = s.db.SQL().Exec(
		`UPDATE backend_configs SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Backend updated"})
}

// ---------------------------------------------------------------- 删除

func (s *Server) handleBackendDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}

	// 删除最后一个 backend 时提示
	var cnt int
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs`).Scan(&cnt)
	if cnt <= 1 {
		s.writeErr(w, http.StatusBadRequest, "Cannot delete the last backend")
		return
	}

	if _, err := s.db.SQL().Exec(`DELETE FROM backend_configs WHERE id = ?`, id); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 若删的是活跃 backend，把第一个设为活跃
	var active int
	_ = s.db.SQL().QueryRow(`SELECT COUNT(*) FROM backend_configs WHERE is_active = 1`).Scan(&active)
	if active == 0 {
		_, _ = s.db.SQL().Exec(`UPDATE backend_configs SET is_active = 1 WHERE id = (SELECT MIN(id) FROM backend_configs)`)
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Backend deleted"})
}

// ---------------------------------------------------------------- 激活 / 监听

func (s *Server) handleBackendActivate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}
	if _, err := s.db.SQL().Exec(`UPDATE backend_configs SET is_active = 0`); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.db.SQL().Exec(`UPDATE backend_configs SET is_active = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "message": "Backend activated"})
}

func (s *Server) handleBackendSetListening(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}
	var req struct {
		Listening *bool `json:"listening"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	on := true
	if req.Listening != nil {
		on = *req.Listening
	}
	if _, err := s.db.SQL().Exec(
		`UPDATE backend_configs SET listening = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		boolToInt(on), id); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"id": id, "listening": on, "message": "Backend updated"})
}

// ---------------------------------------------------------------- 清数据

func (s *Server) handleBackendClearData(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}

	tables := []string{
		"domain_stats", "ip_stats", "proxy_stats", "rule_stats", "rule_proxy_map",
		"country_stats", "device_stats", "device_domain_stats", "device_ip_stats",
		"hourly_stats", "connection_logs", "minute_stats", "minute_dim_stats",
		"minute_country_stats", "hourly_dim_stats", "hourly_country_stats",
		"domain_proxy_stats", "ip_proxy_stats", "rule_chain_traffic",
		"rule_domain_traffic", "rule_ip_traffic", "backend_health_logs",
	}

	tx, err := s.db.SQL().Begin()
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()

	for _, t := range tables {
		if _, err := tx.Exec(`DELETE FROM `+t+` WHERE backend_id = ?`, id); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"message": "Backend data cleared"})
}

// ---------------------------------------------------------------- 测试连通

func (s *Server) handleBackendTest(w http.ResponseWriter, r *http.Request) {
	var in backendInput
	_ = json.NewDecoder(r.Body).Decode(&in)
	s.probeGateway(w, r, in.URL, in.Token, in.normalizeType())
}

func (s *Server) handleBackendTestByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Backend not found")
		return
	}
	s.probeGateway(w, r, be.URL, be.Token, be.Type)
}

// probeGateway 尝试连网关 /version，返回 success/message。
func (s *Server) probeGateway(w http.ResponseWriter, r *http.Request, url, token, typ string) {
	if typ == "agent" {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"message": "Agent backend does not require connectivity test",
		})
		return
	}
	c := gateway.New(url, token, s.cfg.BackendTLSInsecure)
	start := time.Now()
	ver, err := c.Version(r.Context())
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"message": "Connection failed: " + err.Error(),
		})
		return
	}
	ms := time.Since(start).Milliseconds()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Connected to gateway version " + ver + " in " + strconv.FormatInt(ms, 10) + "ms",
		"version": ver,
		"latency": ms,
	})
}

// ---------------------------------------------------------------- agent token

func (s *Server) handleRotateAgentToken(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Invalid backend id")
		return
	}
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeErr(w, http.StatusNotFound, "Backend not found")
		return
	}
	if be.Type != "agent" {
		s.writeErr(w, http.StatusBadRequest, "Only agent backend supports token rotation")
		return
	}
	tok := generateAgentToken()
	if _, err := s.db.SQL().Exec(
		`UPDATE backend_configs SET token = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, tok, id); err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"message":    "Agent token rotated",
		"agentToken": tok,
	})
}

// ---------------------------------------------------------------- 健康历史

func (s *Server) handleBackendHealthHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	idStr := q.Get("backendId")
	var id int64
	if idStr != "" {
		v, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			s.writeErr(w, http.StatusBadRequest, "Invalid backendId")
			return
		}
		id = v
	} else {
		v, err := s.stats.ActiveBackendID()
		if err != nil {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		id = v
	}

	limit := parseLimit(q.Get("limit"), 60, 1440)

	rows, err := s.db.SQL().Query(`
		SELECT minute, status, latency_ms, server_latency_ms, message
		FROM backend_health_logs
		WHERE backend_id = ?
		ORDER BY minute DESC LIMIT ?`, id, limit)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type point struct {
		Time          string  `json:"time"`
		Status        string  `json:"status"`
		LatencyMS     *int64  `json:"latency_ms"`
		ServerLatency *int64  `json:"server_latency_ms"`
		Message       *string `json:"message"`
	}
	pts := []point{}
	for rows.Next() {
		var p point
		var lat, srv sql.NullInt64
		var msg sql.NullString
		if err := rows.Scan(&p.Time, &p.Status, &lat, &srv, &msg); err != nil {
			s.writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if lat.Valid {
			p.LatencyMS = &lat.Int64
		}
		if srv.Valid {
			p.ServerLatency = &srv.Int64
		}
		if msg.Valid {
			p.Message = &msg.String
		}
		pts = append(pts, p)
	}

	var name string
	_ = s.db.SQL().QueryRow(`SELECT name FROM backend_configs WHERE id = ?`, id).Scan(&name)

	s.writeJSON(w, http.StatusOK, map[string]any{
		"backendId":   id,
		"backendName": name,
		"points":      pts,
	})
}

// ---------------------------------------------------------------- 工具

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// generateAgentToken 生成 agent 用随机 token（32 hex 字符）。
func generateAgentToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "ag_" + hex.EncodeToString(b)
}
