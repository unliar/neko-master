package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/gateway"
)

// handleProviderRefresh 对应 POST /api/gateway/providers/proxies/refresh。
//
// 上游校验顺序：
//  1. backendId 解析失败 → 404
//  2. backend 不存在或 type != surge → 400 "Only Surge backend supports this operation"
//  3. backend 是 agent 模式 → 400 "Agent mode backend does not support this operation"
func (s *Server) handleProviderRefresh(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	be, err := s.stats.GetBackend(id)
	if err != nil {
		s.writeErr(w, http.StatusBadRequest, "Only Surge backend supports this operation")
		return
	}

	if strings.HasPrefix(strings.ToLower(be.URL), "agent://") {
		s.writeErr(w, http.StatusBadRequest, "Agent mode backend does not support this operation")
		return
	}
	if be.Type != "surge" {
		s.writeErr(w, http.StatusBadRequest, "Only Surge backend supports this operation")
		return
	}

	// Surge 没有 provider 刷新概念，这里做一次 policies 拉取并刷新缓存
	c := gateway.New(be.URL, be.Token, s.cfg.BackendTLSInsecure)
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	providers, err := c.Providers(ctx)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{
			"success": false,
			"message": "Failed to fetch policies: " + err.Error(),
			"updated": 0,
		})
		return
	}

	// 清掉 surge 策略缓存
	_, _ = s.db.SQL().Exec(`DELETE FROM surge_policy_cache WHERE backend_id = ?`, id)

	updated := 0
	if provs, ok := providers["providers"].(map[string]any); ok {
		for name := range provs {
			_, _ = s.db.SQL().Exec(`
				INSERT INTO surge_policy_cache (backend_id, policy_group, selected_policy, policy_type)
				VALUES (?, ?, '', '')
				ON CONFLICT(backend_id, policy_group) DO UPDATE SET updated_at = CURRENT_TIMESTAMP`,
				id, name)
			updated++
		}
	}

	s.writeJSON(w, http.StatusOK, map[string]any{
		"success": true,
		"message": "Policies refreshed",
		"updated": updated,
	})
}

var _ = json.Marshal
