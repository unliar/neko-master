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

// wsMessage 是客户端 → 服务端的消息（只保留实现用到的字段）。
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
	IncludeProxyDetails *bool    `json:"includeProxyDetails"`
	ProxyChain          string   `json:"proxyChain"`
	IncludeRuleDetails  *bool    `json:"includeRuleDetails"`
	RuleName            string   `json:"ruleName"`
	IncludeRuleChain    *bool    `json:"includeRuleChainFlow"`
	IncludeDomainsPage  *bool    `json:"includeDomainsPage"`
	IncludeIPsPage      *bool    `json:"includeIPsPage"`
	IncludeLiveConns    *bool    `json:"includeLiveConnections"`
}

// wsSubscription 记录一条连接的订阅状态。
type wsSubscription struct {
	backendID     int64
	start         *time.Time
	end           *time.Time
	minInterval   time.Duration
	includeSumm   bool
	includeTrend  bool
	trendMinutes  int
	includeDev    bool
	deviceSource  string
	includeProxy  bool
	proxyChain    string
	includeRule   bool
	ruleName      string
	includeDomPag bool
	includeIPsPag bool
	lastPush      time.Time
	lastSig       string
}

func newSubscription() *wsSubscription {
	return &wsSubscription{
		minInterval:  1000 * time.Millisecond,
		includeSumm:  true,
		includeTrend: true,
		trendMinutes: 30,
	}
}

// handleWS 处理 /ws 升级与消息循环。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	// 鉴权：若开启了访问控制，校验 token（query 或 cookie）
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

	// 读循环放在独立 goroutine，通过 channel 传递消息
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
					// 立即推一次
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
		if ck, err := r.Cookie("neko-session"); err == nil {
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

	// start/end：两者都给且合法才更新
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
		max := 30 * 24 * 60
		if v > max {
			v = max
		}
		sub.trendMinutes = v
	}
	if m.IncludeDeviceDetail != nil {
		sub.includeDev = *m.IncludeDeviceDetail
	}
	if m.DeviceSourceIP != "" {
		sub.deviceSource = m.DeviceSourceIP
	}
	if m.IncludeProxyDetails != nil {
		sub.includeProxy = *m.IncludeProxyDetails
	}
	if m.ProxyChain != "" {
		sub.proxyChain = m.ProxyChain
	}
	if m.IncludeRuleDetails != nil {
		sub.includeRule = *m.IncludeRuleDetails
	}
	if m.RuleName != "" {
		sub.ruleName = m.RuleName
	}
	if m.IncludeDomainsPage != nil {
		sub.includeDomPag = *m.IncludeDomainsPage
	}
	if m.IncludeIPsPage != nil {
		sub.includeIPsPag = *m.IncludeIPsPage
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

	data, err := s.buildStatsSummary(sub)
	if err != nil {
		s.log.Warn("ws build summary", "backend", sub.backendID, "err", err)
		return
	}

	// 内容签名：无变化不推（对应上游 subscribeNoop）
	sig := fmt.Sprintf("%d|%d|%d|%d|%d", data.TotalUpload, data.TotalDownload,
		data.TotalConnections, data.TopDomainCount, data.RuleStatCount)
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

// buildStatsSummary 组装 WS 推送用的 StatsSummary。
func (s *Server) buildStatsSummary(sub *wsSubscription) (*model.StatsSummary, error) {
	sum, err := s.stats.Summary(sub.backendID)
	if err != nil {
		return nil, err
	}
	out := &model.StatsSummary{
		TotalConnections: sum.TotalConnections,
		TotalUpload:      sum.TotalUpload,
		TotalDownload:    sum.TotalDownload,
		TotalDomains:     sum.TotalDomains,
		TotalIPs:         sum.TotalIPs,
		TotalRules:       sum.TotalRules,
		TotalProxies:     sum.TotalProxies,
		TopDomains:       sum.TopDomains,
		TopIPs:           sum.TopIPs,
		ProxyStats:       sum.ProxyStats,
		RuleStats:        sum.RuleStats,
		HourlyStats:      sum.HourlyStats,
	}
	out.TopDomainCount = len(sum.TopDomains)
	out.RuleStatCount = len(sum.RuleStats)
	return out, nil
}
