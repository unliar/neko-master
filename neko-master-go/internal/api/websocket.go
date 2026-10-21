package api

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
	"github.com/unliar/neko-master/neko-master-go/internal/stats"
)

// 说明：这里手写最小可用的 WebSocket 服务端（RFC 6455 文本帧），
// 避免为单一功能引入第三方依赖。上游协议见 API-DETAILS.md §5。

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// wsConn 是一个已升级的连接。
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu sync.Mutex
}

// wsFrame 表示一个解码后的帧。
type wsFrame struct {
	opcode  byte
	payload []byte
}

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// upgrade 执行 WebSocket 握手。
func upgrade(w http.ResponseWriter, r *http.Request) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return nil, errors.New("not a websocket upgrade request")
	}
	if !headerContains(r.Header, "Connection", "upgrade") {
		return nil, errors.New("connection header missing upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, errors.New("missing Sec-WebSocket-Key")
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer does not support hijack")
	}
	netConn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	accept := computeAcceptKey(key)
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		netConn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		netConn.Close()
		return nil, err
	}

	return &wsConn{conn: netConn, br: rw.Reader}, nil
}

func headerContains(h http.Header, key, sub string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), sub) {
				return true
			}
		}
	}
	return false
}

func computeAcceptKey(key string) string {
	h := sha1.New()
	io.WriteString(h, key)
	io.WriteString(h, wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// readFrame 读取一个完整帧（处理分片）。
func (c *wsConn) readFrame() (*wsFrame, error) {
	var assembled []byte
	var opcode byte

	for {
		b0, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}
		b1, err := c.br.ReadByte()
		if err != nil {
			return nil, err
		}

		fin := b0&0x80 != 0
		op := b0 & 0x0F
		masked := b1&0x80 != 0
		length := int64(b1 & 0x7F)

		switch length {
		case 126:
			var ext uint16
			if err := binary.Read(c.br, binary.BigEndian, &ext); err != nil {
				return nil, err
			}
			length = int64(ext)
		case 127:
			var ext uint64
			if err := binary.Read(c.br, binary.BigEndian, &ext); err != nil {
				return nil, err
			}
			if ext > 1<<24 {
				return nil, errors.New("frame too large")
			}
			length = int64(ext)
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, maskKey[:]); err != nil {
				return nil, err
			}
		}

		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}

		if op != opContinuation {
			opcode = op
		}
		assembled = append(assembled, payload...)

		if fin {
			break
		}
		if len(assembled) > 1<<24 {
			return nil, errors.New("message too large")
		}
	}

	return &wsFrame{opcode: opcode, payload: assembled}, nil
}

// writeFrame 写一个帧。
func (c *wsConn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	var header []byte
	header = append(header, 0x80|opcode)

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 65536:
		header = append(header, 126, byte(n>>8), byte(n))
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}

	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// WriteJSON 写一个 JSON 文本帧。
func (c *wsConn) WriteJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(opText, b)
}

// Close 关闭连接。
func (c *wsConn) Close() error {
	_ = c.writeFrame(opClose, nil)
	return c.conn.Close()
}

// wsMessage 是客户端 → 服务端的消息。
//
// 字段与上游 websocket.server.ts 内联版一致；
// 特别注意 ipsPageSortBy 是 string（上游 types 文件里写成 number 是笔误）。
type wsMessage struct {
	Type                string   `json:"type"`
	BackendID           *int64   `json:"backendId"`
	Start               string   `json:"start"`
	End                 string   `json:"end"`
	MinPushIntervalMs   *int     `json:"minPushIntervalMs"`
	IncludeSummary      *bool    `json:"includeSummary"`
	SummaryFields       []string `json:"summaryFields"`
	IncludeTrend        *bool    `json:"includeTrend"`
	TrendMinutes        *int     `json:"trendMinutes"`
	TrendBucketMinutes  *int     `json:"trendBucketMinutes"`
	IncludeDeviceDetail *bool    `json:"includeDeviceDetails"`
	DeviceSourceIP      string   `json:"deviceSourceIP"`
	DeviceDetailLimit   *int     `json:"deviceDetailLimit"`
	IncludeProxyDetails *bool    `json:"includeProxyDetails"`
	ProxyChain          string   `json:"proxyChain"`
	ProxyDetailLimit    *int     `json:"proxyDetailLimit"`
	IncludeRuleDetails  *bool    `json:"includeRuleDetails"`
	RuleName            string   `json:"ruleName"`
	RuleDetailLimit     *int     `json:"ruleDetailLimit"`
	IncludeRuleChain    *bool    `json:"includeRuleChainFlow"`

	IncludeDomainsPage   *bool   `json:"includeDomainsPage"`
	DomainsPageOffset    *int    `json:"domainsPageOffset"`
	DomainsPageLimit     *int    `json:"domainsPageLimit"`
	DomainsPageSortBy    *string `json:"domainsPageSortBy"`
	DomainsPageSortOrder *string `json:"domainsPageSortOrder"`
	DomainsPageSearch    *string `json:"domainsPageSearch"`

	IncludeIPsPage   *bool   `json:"includeIPsPage"`
	IPsPageOffset    *int    `json:"ipsPageOffset"`
	IPsPageLimit     *int    `json:"ipsPageLimit"`
	IPsPageSortBy    *string `json:"ipsPageSortBy"`
	IPsPageSortOrder *string `json:"ipsPageSortOrder"`
	IPsPageSearch    *string `json:"ipsPageSearch"`
}

// wsSubscription 记录一条连接的订阅状态。
type wsSubscription struct {
	backendID    int64
	start        *time.Time
	end          *time.Time
	minInterval  time.Duration
	includeSumm  bool
	includeTrend bool
	trendMinutes int
	trendBucket  int
	includeDev   bool
	deviceSource string
	deviceLimit  int
	includeProxy bool
	proxyChain   string
	proxyLimit   int
	includeRule  bool
	ruleName     string
	ruleLimit    int
	includeChain bool

	hasDomPage bool
	domPage    stats.PageParams
	hasIPsPage bool
	ipsPage    stats.PageParams

	lastPush time.Time
	lastSig  string
}

func newSubscription() *wsSubscription {
	return &wsSubscription{
		minInterval:  1000 * time.Millisecond,
		includeSumm:  true,
		includeTrend: true,
		trendMinutes: 30,
		trendBucket:  1,
		deviceLimit:  5000,
		proxyLimit:   5000,
		ruleLimit:    5000,
	}
}

// handleWS 处理 /ws 升级与消息循环。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !s.wsAuthorized(r) {
		s.writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	c, err := upgrade(w, r)
	if err != nil {
		s.log.Debug("ws upgrade failed", "err", err)
		return
	}
	defer c.Close()

	sub := newSubscription()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	msgs := make(chan *wsFrame, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			f, err := c.readFrame()
			if err != nil {
				errCh <- err
				return
			}
			msgs <- f
		}
	}()

	for {
		select {
		case <-r.Context().Done():
			return

		case err := <-errCh:
			s.log.Debug("ws read closed", "err", err)
			return

		case f := <-msgs:
			switch f.opcode {
			case opClose:
				return
			case opPing:
				_ = c.writeFrame(opPong, f.payload)
			case opText:
				var m wsMessage
				if err := json.Unmarshal(f.payload, &m); err != nil {
					continue
				}
				s.applyWSMessage(sub, &m)
				if m.Type == "ping" {
					_ = c.WriteJSON(map[string]any{
						"type":      "pong",
						"timestamp": time.Now().UTC().Format(time.RFC3339),
					})
					continue
				}
				if m.Type == "subscribe" {
					sub.lastSig = ""
					s.pushStats(c, sub)
				}
			}

		case <-tick.C:
			s.pushStats(c, sub)
		}
	}
}

// wsAuthorized 校验 WS 鉴权。
func (s *Server) wsAuthorized(r *http.Request) bool {
	if s.cfg.ForceAccessControlOff {
		return true
	}
	enabled, ok := s.authEnabled()
	if !ok || !enabled {
		return true
	}
	tok := r.URL.Query().Get("token")
	if tok == "" {
		if ck, err := r.Cookie(sessionCookie); err == nil {
			tok = ck.Value
		}
	}
	return s.verifyToken(tok)
}

// applyWSMessage 把订阅消息合并进当前订阅状态。
func (s *Server) applyWSMessage(sub *wsSubscription, m *wsMessage) {
	if m.BackendID != nil {
		if _, err := s.stats.GetBackend(*m.BackendID); err == nil {
			sub.backendID = *m.BackendID
		}
	}
	if sub.backendID == 0 {
		if id, err := s.stats.ActiveBackendID(); err == nil {
			sub.backendID = id
		}
	}

	// start/end：两者都给且合法才更新；都为空表示全量
	if m.Start != "" && m.End != "" {
		st, err1 := time.Parse(time.RFC3339, m.Start)
		en, err2 := time.Parse(time.RFC3339, m.End)
		if err1 == nil && err2 == nil && !st.After(en) {
			sub.start, sub.end = &st, &en
		}
	} else if m.Start == "" && m.End == "" {
		sub.start, sub.end = nil, nil
	}

	if m.MinPushIntervalMs != nil {
		v := *m.MinPushIntervalMs
		if v < 0 {
			v = 0
		}
		if v > 60000 {
			v = 60000
		}
		sub.minInterval = time.Duration(v) * time.Millisecond
	}

	if m.IncludeSummary != nil {
		sub.includeSumm = *m.IncludeSummary
	}
	if m.IncludeTrend != nil {
		sub.includeTrend = *m.IncludeTrend
	}
	if m.TrendMinutes != nil {
		v := *m.TrendMinutes
		if v <= 0 {
			v = 30
		}
		if v > 30*24*60 {
			v = 30 * 24 * 60
		}
		sub.trendMinutes = v
	}
	if m.TrendBucketMinutes != nil {
		v := *m.TrendBucketMinutes
		if v <= 0 {
			v = 1
		}
		if v > 24*60 {
			v = 24 * 60
		}
		sub.trendBucket = v
	}

	// 设备下钻
	if m.IncludeDeviceDetail != nil {
		if !*m.IncludeDeviceDetail {
			sub.includeDev = false
		} else if m.DeviceSourceIP != "" {
			sub.includeDev = true
			sub.deviceSource = m.DeviceSourceIP
		}
	}
	if m.DeviceSourceIP != "" {
		sub.deviceSource = m.DeviceSourceIP
	}
	if m.DeviceDetailLimit != nil {
		v := *m.DeviceDetailLimit
		if v > 20000 {
			v = 20000
		}
		if v <= 0 {
			v = 5000
		}
		sub.deviceLimit = v
	}

	// 代理下钻
	if m.IncludeProxyDetails != nil {
		if !*m.IncludeProxyDetails {
			sub.includeProxy = false
		} else if m.ProxyChain != "" {
			sub.includeProxy = true
			sub.proxyChain = m.ProxyChain
		}
	}
	if m.ProxyChain != "" {
		sub.proxyChain = m.ProxyChain
	}
	if m.ProxyDetailLimit != nil {
		v := *m.ProxyDetailLimit
		if v > 20000 {
			v = 20000
		}
		if v <= 0 {
			v = 5000
		}
		sub.proxyLimit = v
	}

	// 规则下钻
	if m.IncludeRuleDetails != nil {
		if !*m.IncludeRuleDetails {
			sub.includeRule = false
		} else if m.RuleName != "" {
			sub.includeRule = true
			sub.ruleName = m.RuleName
		}
	}
	if m.RuleName != "" {
		sub.ruleName = m.RuleName
	}
	if m.RuleDetailLimit != nil {
		v := *m.RuleDetailLimit
		if v > 20000 {
			v = 20000
		}
		if v <= 0 {
			v = 5000
		}
		sub.ruleLimit = v
	}
	if m.IncludeRuleChain != nil {
		sub.includeChain = *m.IncludeRuleChain
	}

	// 分页：任一参数非法则整个 page 设置被忽略（不应用也不清空）
	if m.IncludeDomainsPage != nil && *m.IncludeDomainsPage {
		if p, ok := stats.NormDomainPage(m.DomainsPageOffset, m.DomainsPageLimit,
			m.DomainsPageSortBy, m.DomainsPageSortOrder, m.DomainsPageSearch); ok {
			sub.hasDomPage = true
			sub.domPage = p
		}
	} else if m.IncludeDomainsPage != nil && !*m.IncludeDomainsPage {
		sub.hasDomPage = false
	}

	if m.IncludeIPsPage != nil && *m.IncludeIPsPage {
		if p, ok := stats.NormIPPage(m.IPsPageOffset, m.IPsPageLimit,
			m.IPsPageSortBy, m.IPsPageSortOrder, m.IPsPageSearch); ok {
			sub.hasIPsPage = true
			sub.ipsPage = p
		}
	} else if m.IncludeIPsPage != nil && !*m.IncludeIPsPage {
		sub.hasIPsPage = false
	}
}

// pushStats 生成并推送一帧 stats。
func (s *Server) pushStats(c *wsConn, sub *wsSubscription) {
	if sub.backendID == 0 {
		return
	}
	if !sub.lastPush.IsZero() && time.Since(sub.lastPush) < sub.minInterval {
		return
	}

	data, sig, err := s.buildStatsSummary(sub)
	if err != nil {
		s.log.Warn("ws build summary", "backend", sub.backendID, "err", err)
		return
	}

	// 内容签名：无变化不推（对应上游 subscribeNoop）
	if sig == sub.lastSig {
		return
	}
	sub.lastSig = sig
	sub.lastPush = time.Now()

	frame := map[string]any{
		"type":      "stats",
		"backendId": sub.backendID,
		"data":      data,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	}
	if sub.start != nil {
		frame["start"] = sub.start.UTC().Format(time.RFC3339)
	}
	if sub.end != nil {
		frame["end"] = sub.end.UTC().Format(time.RFC3339)
	}
	if sub.includeSumm {
		frame["summaryFields"] = []string{
			"totals", "topDomains", "topIPs", "proxyStats",
			"countryStats", "deviceStats", "ruleStats", "hourlyStats",
		}
	}

	if err := c.WriteJSON(frame); err != nil {
		s.log.Debug("ws write failed", "err", err)
	}
}

// buildStatsSummary 按订阅参数组装完整 StatsSummary，同时返回变更签名。
func (s *Server) buildStatsSummary(sub *wsSubscription) (*model.StatsSummary, string, error) {
	out := &model.StatsSummary{}

	sum, err := s.stats.Summary(sub.backendID)
	if err != nil {
		return nil, "", err
	}
	out.TotalUpload = sum.TotalUpload
	out.TotalDownload = sum.TotalDownload
	out.TotalConnections = sum.TotalConnections
	out.TotalDomains = sum.TotalDomains
	out.TotalIPs = sum.TotalIPs
	out.TotalProxies = sum.TotalProxies

	need := func(k string) bool { return true } // Phase 1 简化：全量下发

	_ = need

	if sum.TotalRules > 0 {
		v := sum.TotalRules
		out.TotalRules = &v
	}
	if sum.TodayUpload > 0 {
		v := sum.TodayUpload
		out.TodayUpload = &v
	}
	if sum.TodayDownload > 0 {
		v := sum.TodayDownload
		out.TodayDownload = &v
	}

	out.TopDomains = toAnySlice(sum.TopDomains)
	out.TopIPs = s.attachGeoIPs(toAnySlice(sum.TopIPs))
	out.ProxyStats = toAnySlice(sum.ProxyStats)
	out.RuleStats = toAnySlice(sum.RuleStats)
	out.HourlyStats = toAnySlice(sum.HourlyStats)

	out.TopDomainCount = len(sum.TopDomains)
	out.RuleStatCount = len(sum.RuleStats)

	if cs, err := s.stats.Countries(sub.backendID, 50, 0); err == nil && len(cs) > 0 {
		out.CountryStats = toAnySlice(cs)
	}
	if ds, err := s.stats.Devices(sub.backendID, 50, 0); err == nil && len(ds) > 0 {
		out.DeviceStats = toAnySlice(ds)
	}

	// 趋势
	if sub.includeTrend {
		if pts, err := s.stats.TrendAggregated(sub.backendID, sub.trendMinutes, sub.trendBucket, 0, nil, nil); err == nil && len(pts) > 0 {
			out.TrendStats = pts
		}
	}

	// 设备下钻
	if sub.includeDev && sub.deviceSource != "" {
		src := sub.deviceSource
		out.DeviceDetailSourceIP = &src
		if doms, err := s.stats.DeviceDomains(sub.backendID, src, sub.deviceLimit); err == nil {
			out.DeviceDomains = toAnySlice(doms)
		}
		if ips, err := s.stats.DeviceIPs(sub.backendID, src, sub.deviceLimit); err == nil {
			out.DeviceIPs = s.attachGeoIPs(toAnySlice(ips))
		}
	}

	// 代理下钻
	if sub.includeProxy && sub.proxyChain != "" {
		ch := sub.proxyChain
		out.ProxyDetailChain = &ch
		if doms, err := s.stats.ProxyDomains(sub.backendID, ch, sub.proxyLimit); err == nil {
			out.ProxyDomains = toAnySlice(doms)
		}
		if ips, err := s.stats.ProxyIPs(sub.backendID, ch, sub.proxyLimit); err == nil {
			out.ProxyIPs = s.attachGeoIPs(toAnySlice(ips))
		}
	}

	// 规则下钻
	if sub.includeRule && sub.ruleName != "" {
		rn := sub.ruleName
		out.RuleDetailName = &rn
		if doms, err := s.stats.RuleDomains(sub.backendID, rn, sub.ruleLimit); err == nil {
			out.RuleDomains = toAnySlice(doms)
		}
		if ips, err := s.stats.RuleIPs(sub.backendID, rn, sub.ruleLimit); err == nil {
			out.RuleIPs = s.attachGeoIPs(toAnySlice(ips))
		}
	}

	// 分页
	if sub.hasDomPage {
		if res, err := s.stats.DomainsPage(sub.backendID, sub.domPage); err == nil {
			out.DomainsPage = &model.PageResult{Data: s.attachGeoIPs(res.Data), Total: res.Total}
			q := sub.domPage
			order := q.SortOrder
			by := reverseSortKey(q.SortBy)
			out.DomainsPageQuery = &model.PageQuery{
				Offset: q.Offset, Limit: q.Limit,
				SortBy: by, SortOrder: order, Search: q.Search,
			}
		}
	}
	if sub.hasIPsPage {
		if res, err := s.stats.IPsPage(sub.backendID, sub.ipsPage); err == nil {
			out.IPsPage = &model.PageResult{Data: s.attachGeoIPs(res.Data), Total: res.Total}
			q := sub.ipsPage
			out.IPsPageQuery = &model.PageQuery{
				Offset: q.Offset, Limit: q.Limit,
				SortBy: reverseSortKey(q.SortBy), SortOrder: q.SortOrder, Search: q.Search,
			}
		}
	}

	sig := fmt.Sprintf("%d|%d|%d|%d|%d|%d|%d|%v|%v|%d|%d|%v",
		out.TotalUpload, out.TotalDownload, out.TotalConnections,
		len(out.CountryStats), len(out.DeviceStats), len(out.TrendStats),
		out.TopDomainCount,
		sub.includeDev, sub.includeProxy, sub.ruleLimit,
		pageTotal(out.DomainsPage), pageTotal(out.IPsPage))

	return out, sig, nil
}

// attachGeoIPs 给一组 IP 记录补 GeoIP。
//
// 传入元素是 stats.IPRow 结构体而非 map，必须先序列化再附加字段；
// 直接断言 map[string]any 会永远失败，表现为「geoIP 一个都不出现」。
func (s *Server) attachGeoIPs(rows []any) []any {
	if s.geo == nil || len(rows) == 0 {
		return rows
	}
	for i, r := range rows {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		ip, _ := m["ip"].(string)
		if ip == "" {
			continue
		}
		if info := s.geo.IP(ip); info != nil {
			m["geoIP"] = info
		}
		rows[i] = m
	}
	return rows
}

func pageTotal(p *model.PageResult) int {
	if p == nil {
		return -1
	}
	return p.Total
}

func toAnySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// reverseSortKey 把 DB 列名映射回前端用的 key。
func reverseSortKey(col string) string {
	switch col {
	case "total_upload":
		return "upload"
	case "total_download":
		return "download"
	case "(total_upload + total_download)":
		return "traffic"
	case "total_connections":
		return "conns"
	case "last_seen":
		return "lastSeen"
	default:
		return col
	}
}
