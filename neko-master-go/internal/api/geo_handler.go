package api

import (
	"net/http"
)

// handleGeoStatus 返回 GeoIP 数据源状态。
//
// 上游没有这个路由，但前端设置页需要知道 MMDB 有没有加载成功，
// 所以补一个只读的诊断接口。
func (s *Server) handleGeoStatus(w http.ResponseWriter, r *http.Request) {
	if s.geo == nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"provider": "disabled"})
		return
	}
	s.writeJSON(w, http.StatusOK, s.geo.Stats())
}

// handleGeoLookup 手动查一个 IP 的归属，便于调试。
func (s *Server) handleGeoLookup(w http.ResponseWriter, r *http.Request) {
	ip := r.URL.Query().Get("ip")
	if ip == "" {
		s.writeErr(w, http.StatusBadRequest, "ip parameter is required")
		return
	}
	if s.geo == nil {
		s.writeJSON(w, http.StatusOK, nil)
		return
	}
	info := s.geo.IP(ip)
	s.writeJSON(w, http.StatusOK, info)
}
