// Package model 定义 API 与存储层共用的数据类型。
//
// 所有类型都严格对齐 neko-master 前端所期望的 JSON 结构，
// 参见仓库根目录 API-DETAILS.md。
package model

// Backend 对应 backend_configs 表，表示一个被监控的网关实例。
type Backend struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	URL       string `json:"url"`
	Token     string `json:"-"`    // 绝不外泄
	Type      string `json:"type"` // clash | surge | agent
	Enabled   bool   `json:"enabled"`
	IsActive  bool   `json:"isActive"`
	Listening bool   `json:"listening"`
	CreatedAt string `json:"createdAt,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// DomainStat 域名维度累计统计。
type DomainStat struct {
	Domain           string `json:"domain"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
	IPs              string `json:"ips,omitempty"`
	Rules            string `json:"rules,omitempty"`
	Chains           string `json:"chains,omitempty"`
}

// IPStat IP 维度累计统计。
type IPStat struct {
	IP               string `json:"ip"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
	Domains          string `json:"domains,omitempty"`
	ASN              string `json:"asn,omitempty"`
	GeoIP            string `json:"geoip,omitempty"`
	Chains           string `json:"chains,omitempty"`
	Rules            string `json:"rules,omitempty"`
}

// ProxyStat 代理链路维度累计统计。
type ProxyStat struct {
	Chain            string `json:"chain"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
}

// RuleStat 规则维度累计统计。
type RuleStat struct {
	Rule             string `json:"rule"`
	FinalProxy       string `json:"finalProxy,omitempty"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
}

// DeviceStat 设备（源 IP）维度累计统计。
type DeviceStat struct {
	SourceIP         string `json:"sourceIp"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
}

// CountryStats 国家维度累计统计。
//
// 注意：源码中存在两个冲突定义（stats.types.ts 无 lastSeen，
// shared/index.ts 有可选 lastSeen）。此处按 shared 输出。
type CountryStats struct {
	Country          string `json:"country"`
	CountryName      string `json:"countryName,omitempty"`
	Continent        string `json:"continent,omitempty"`
	Upload           int64  `json:"upload"`
	Download         int64  `json:"download"`
	TotalConnections int64  `json:"totalConnections"`
	LastSeen         string `json:"lastSeen,omitempty"`
}

// TrendPoint 趋势序列上的一个点。
type TrendPoint struct {
	Timestamp string `json:"timestamp"`
	Upload    int64  `json:"upload"`
	Download  int64  `json:"download"`
	Total     int64  `json:"total"`
}

// Summary 总览响应。
//
// 注意：源码中 totalConnections/Upload/Download 走 summaryWithRealtime
// （叠加实时缓冲），而 totalDomains/totalIPs 走 summary（不叠加）。
// 这里保持一致，不要统一叠加，否则数值会变。
type Summary struct {
	TotalUpload      int64 `json:"totalUpload"`
	TotalDownload    int64 `json:"totalDownload"`
	TotalConnections int64 `json:"totalConnections"`
	TotalDomains     int64 `json:"totalDomains"`
	TotalIPs         int64 `json:"totalIPs"`
	TotalProxies     int64 `json:"totalProxies"`
	TotalRules       int64 `json:"totalRules"`
}

// Connection 是上游 Mihomo /connections 返回的连接条目（只保留用到的字段）。
type Connection struct {
	ID          string             `json:"id"`
	Metadata    ConnectionMetadata `json:"metadata"`
	Upload      int64              `json:"upload"`
	Download    int64              `json:"download"`
	Chains      []string           `json:"chains"`
	Rule        string             `json:"rule"`
	RulePayload string             `json:"rulePayload"`
	Start       string             `json:"start"`
}

// ConnectionMetadata 连接元数据。
type ConnectionMetadata struct {
	Network         string `json:"network"`
	Type            string `json:"type"`
	SourceIP        string `json:"sourceIP"`
	DestinationIP   string `json:"destinationIP"`
	SourcePort      string `json:"sourcePort"`
	DestinationPort string `json:"destinationPort"`
	Host            string `json:"host"`
	Process         string `json:"process"`
}

// ConnectionsResponse 是 /connections 的完整响应。
type ConnectionsResponse struct {
	DownloadTotal int64        `json:"downloadTotal"`
	UploadTotal   int64        `json:"uploadTotal"`
	Connections   []Connection `json:"connections"`
}

// HealthResponse /health 响应。
type HealthResponse struct {
	Status string `json:"status"`
}
