// Package gateway 负责与上游 Mihomo / Clash 控制接口通信。
//
// 上游实现见 apps/collector/src/modules/collector/ 与 backend/。
// 采集入口是 GET /connections（Mihomo 返回全量连接累计值），
// 我们用两次快照做差得到增量。
package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// Client 是对单个网关的客户端。
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New 创建客户端。baseURL 形如 http://192.168.1.1:9090。
// insecure 为 true 时跳过 TLS 证书校验（对应 BACKEND_TLS_INSECURE）。
func New(baseURL, token string, insecure bool) *Client {
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		baseURL = "http://" + baseURL
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	tr := &http.Transport{
		Proxy:                 nil, // 绝不走系统代理，直连网关
		MaxIdleConns:          10,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}
	if insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Transport: tr, Timeout: 15 * time.Second},
	}
}

// BaseURL 返回规范化后的地址。
func (c *Client) BaseURL() string { return c.baseURL }

func (c *Client) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gateway %s: HTTP %d", path, resp.StatusCode)
	}
	return body, nil
}

// ErrAuth 表示网关鉴权失败。
var ErrAuth = fmt.Errorf("gateway auth failed")

// Version 拉取 /version，用于健康探测。
func (c *Client) Version(ctx context.Context) (string, error) {
	b, err := c.get(ctx, "/version")
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}

// Connections 拉取 /connections 全量快照。
func (c *Client) Connections(ctx context.Context) (*model.ConnectionsResponse, error) {
	b, err := c.get(ctx, "/connections")
	if err != nil {
		return nil, err
	}
	var r model.ConnectionsResponse
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("decode /connections: %w", err)
	}
	return &r, nil
}

// Proxies 拉取 /proxies。
func (c *Client) Proxies(ctx context.Context) (map[string]any, error) {
	b, err := c.get(ctx, "/proxies")
	if err != nil {
		return nil, err
	}
	var r struct {
		Proxies map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Proxies == nil {
		r.Proxies = map[string]any{}
	}
	return r.Proxies, nil
}

// Rules 拉取 /rules。
func (c *Client) Rules(ctx context.Context) ([]any, error) {
	b, err := c.get(ctx, "/rules")
	if err != nil {
		return nil, err
	}
	var r struct {
		Rules []any `json:"rules"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return r.Rules, nil
}

// Providers 拉取 /providers/proxies。
func (c *Client) Providers(ctx context.Context) (map[string]any, error) {
	b, err := c.get(ctx, "/providers/proxies")
	if err != nil {
		return nil, err
	}
	var r struct {
		Providers map[string]any `json:"providers"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Providers == nil {
		r.Providers = map[string]any{}
	}
	return r.Providers, nil
}

// RefreshProvider 触发某个 provider 刷新。
func (c *Client) RefreshProvider(ctx context.Context, name string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		c.baseURL+"/providers/proxies/"+name, nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("refresh provider %s: HTTP %d", name, resp.StatusCode)
	}
	return nil
}
