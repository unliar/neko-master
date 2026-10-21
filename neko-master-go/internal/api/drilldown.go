package api

import (
	"net/http"
)

// registerDrilldown 注册全部下钻接口。
//
// 依据前端 lib/api.ts 的实际调用清单，这些是详情页必用接口。
func (s *Server) registerDrilldown() {
	// 域名下钻
	s.mux.HandleFunc("GET /api/stats/domains/proxy-stats", s.needDomain(s.handleDomainProxyStats))
	s.mux.HandleFunc("GET /api/stats/domains/ip-details", s.needDomain(s.handleDomainIPDetails))

	// IP 下钻
	s.mux.HandleFunc("GET /api/stats/ips/proxy-stats", s.needIP(s.handleIPProxyStats))
	s.mux.HandleFunc("GET /api/stats/ips/domain-details", s.needIP(s.handleIPDomainDetails))

	// 代理链下钻
	s.mux.HandleFunc("GET /api/stats/proxies/domains", s.needChain(s.handleProxyDomains))
	s.mux.HandleFunc("GET /api/stats/proxies/ips", s.needChain(s.handleProxyIPs))

	// 规则下钻
	s.mux.HandleFunc("GET /api/stats/rules/domains", s.needRule(s.handleRuleDomains))
	s.mux.HandleFunc("GET /api/stats/rules/ips", s.needRule(s.handleRuleIPs))
	s.mux.HandleFunc("GET /api/stats/rules/domains/proxy-stats", s.handleRuleDomainProxyStats)
	s.mux.HandleFunc("GET /api/stats/rules/ips/proxy-stats", s.handleRuleIPProxyStats)
	s.mux.HandleFunc("GET /api/stats/rules/domains/ip-details", s.handleRuleDomainIPDetails)
	s.mux.HandleFunc("GET /api/stats/rules/ips/domain-details", s.handleRuleIPDomainDetails)

	// 设备下钻
	s.mux.HandleFunc("GET /api/stats/devices/domains", s.needSourceIP(s.handleDeviceDomains))
	s.mux.HandleFunc("GET /api/stats/devices/ips", s.needSourceIP(s.handleDeviceIPs))

	// 趋势（聚合版）
	s.mux.HandleFunc("GET /api/stats/trend/aggregated", s.handleTrendAggregated)
}

// ---------------------------------------------------------------- 参数校验包装
// needDomain 校验 domain 必填。
func (s *Server) needDomain(h func(http.ResponseWriter, *http.Request, int64, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolveBackendID(r)
		if !ok || id == 0 {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		domain := r.URL.Query().Get("domain")
		if domain == "" {
			s.writeErr(w, http.StatusBadRequest, "Domain parameter is required")
			return
		}
		h(w, r, id, domain)
	}
}

// needIP 校验 ip 必填。
func (s *Server) needIP(h func(http.ResponseWriter, *http.Request, int64, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolveBackendID(r)
		if !ok || id == 0 {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		ip := r.URL.Query().Get("ip")
		if ip == "" {
			s.writeErr(w, http.StatusBadRequest, "IP parameter is required")
			return
		}
		h(w, r, id, ip)
	}
}

// needChain 校验 chain 必填。
func (s *Server) needChain(h func(http.ResponseWriter, *http.Request, int64, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolveBackendID(r)
		if !ok || id == 0 {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		chain := r.URL.Query().Get("chain")
		if chain == "" {
			s.writeErr(w, http.StatusBadRequest, "Chain parameter is required")
			return
		}
		h(w, r, id, chain)
	}
}

// needRule 校验 rule 必填。
func (s *Server) needRule(h func(http.ResponseWriter, *http.Request, int64, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
		h(w, r, id, rule)
	}
}

// needSourceIP 校验 sourceIP 必填。
func (s *Server) needSourceIP(h func(http.ResponseWriter, *http.Request, int64, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.resolveBackendID(r)
		if !ok || id == 0 {
			s.writeErr(w, http.StatusNotFound, "No backend specified or active")
			return
		}
		src := r.URL.Query().Get("sourceIP")
		if src == "" {
			s.writeErr(w, http.StatusBadRequest, "sourceIP parameter is required")
			return
		}
		h(w, r, id, src)
	}
}

// ---------------------------------------------------------------- handlers

func (s *Server) handleDomainProxyStats(w http.ResponseWriter, r *http.Request, id int64, domain string) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 5000, 20000)
	res, err := s.stats.DomainProxyStats(id, domain, q.Get("sourceIP"), q.Get("sourceChain"), limit)
	if err != nil {
		s.log.Error("domains/proxy-stats", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDomainIPDetails(w http.ResponseWriter, r *http.Request, id int64, domain string) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 100, 2000)
	res, err := s.stats.DomainIPDetails(id, domain, q.Get("sourceIP"), q.Get("sourceChain"), limit)
	if err != nil {
		s.log.Error("domains/ip-details", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleIPProxyStats(w http.ResponseWriter, r *http.Request, id int64, ip string) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 5000, 20000)
	res, err := s.stats.IPProxyStats(id, ip, q.Get("sourceIP"), q.Get("sourceChain"), limit)
	if err != nil {
		s.log.Error("ips/proxy-stats", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleIPDomainDetails(w http.ResponseWriter, r *http.Request, id int64, ip string) {
	q := r.URL.Query()
	limit := parseLimit(q.Get("limit"), 100, 2000)
	res, err := s.stats.IPDomainDetails(id, ip, q.Get("sourceIP"), q.Get("sourceChain"), limit)
	if err != nil {
		s.log.Error("ips/domain-details", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleProxyDomains(w http.ResponseWriter, r *http.Request, id int64, chain string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.ProxyDomains(id, chain, limit)
	if err != nil {
		s.log.Error("proxies/domains", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleProxyIPs(w http.ResponseWriter, r *http.Request, id int64, chain string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.ProxyIPs(id, chain, limit)
	if err != nil {
		s.log.Error("proxies/ips", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleDomains(w http.ResponseWriter, r *http.Request, id int64, rule string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.RuleDomains(id, rule, limit)
	if err != nil {
		s.log.Error("rules/domains", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleIPs(w http.ResponseWriter, r *http.Request, id int64, rule string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.RuleIPs(id, rule, limit)
	if err != nil {
		s.log.Error("rules/ips", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleDomainProxyStats(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	rule, domain := q.Get("rule"), q.Get("domain")
	if rule == "" || domain == "" {
		s.writeErr(w, http.StatusBadRequest, "Rule and domain parameters are required")
		return
	}
	_, _, hasRange, msg := s.getTimeRange(r)
	if msg != "" {
		s.writeErr(w, http.StatusBadRequest, msg)
		return
	}
	res, err := s.stats.RuleDomainProxyStats(id, rule, domain, hasRange)
	if err != nil {
		s.log.Error("rules/domains/proxy-stats", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleIPProxyStats(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	rule, ip := q.Get("rule"), q.Get("ip")
	if rule == "" || ip == "" {
		s.writeErr(w, http.StatusBadRequest, "Rule and IP parameters are required")
		return
	}
	_, _, hasRange, msg := s.getTimeRange(r)
	if msg != "" {
		s.writeErr(w, http.StatusBadRequest, msg)
		return
	}
	res, err := s.stats.RuleIPProxyStats(id, rule, ip, hasRange)
	if err != nil {
		s.log.Error("rules/ips/proxy-stats", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleDomainIPDetails(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	rule, domain := q.Get("rule"), q.Get("domain")
	if rule == "" || domain == "" {
		s.writeErr(w, http.StatusBadRequest, "Rule and domain parameters are required")
		return
	}
	limit := parseLimit(q.Get("limit"), 100, 2000)
	res, err := s.stats.RuleDomainIPDetails(id, rule, domain, limit)
	if err != nil {
		s.log.Error("rules/domains/ip-details", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRuleIPDomainDetails(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	q := r.URL.Query()
	rule, ip := q.Get("rule"), q.Get("ip")
	if rule == "" || ip == "" {
		s.writeErr(w, http.StatusBadRequest, "Rule and IP parameters are required")
		return
	}
	limit := parseLimit(q.Get("limit"), 100, 2000)
	res, err := s.stats.RuleIPDomainDetails(id, rule, ip, limit)
	if err != nil {
		s.log.Error("rules/ips/domain-details", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeviceDomains(w http.ResponseWriter, r *http.Request, id int64, sourceIP string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.DeviceDomains(id, sourceIP, limit)
	if err != nil {
		s.log.Error("devices/domains", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleDeviceIPs(w http.ResponseWriter, r *http.Request, id int64, sourceIP string) {
	limit := parseLimit(r.URL.Query().Get("limit"), 5000, 20000)
	res, err := s.stats.DeviceIPs(id, sourceIP, limit)
	if err != nil {
		s.log.Error("devices/ips", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}

// handleTrendAggregated 对应 GET /api/stats/trend/aggregated。
func (s *Server) handleTrendAggregated(w http.ResponseWriter, r *http.Request) {
	id, ok := s.resolveBackendID(r)
	if !ok || id == 0 {
		s.writeErr(w, http.StatusNotFound, "No backend specified or active")
		return
	}
	start, end, active, msg := s.getTimeRange(r)
	if msg != "" {
		s.writeErr(w, http.StatusBadRequest, msg)
		return
	}
	if !active {
		end = nowFunc()
		start = end.Add(-24 * hourDur)
	}
	res, err := s.stats.Trend(id, start, end)
	if err != nil {
		s.log.Error("trend/aggregated", "err", err)
		s.writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, res)
}
