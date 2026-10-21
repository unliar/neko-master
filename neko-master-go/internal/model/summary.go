package model

// StatsSummary 是 WebSocket 推送帧与 DTO 的完整载荷。
//
// 字段名严格对齐 packages/shared/src/index.ts 的 StatsSummary。
// 所有切片字段用 omitempty，Go 零值 nil 序列化为 null，
// 而 TS 的 optional 字段缺省是 undefined（JSON 中不出现）——
// 因此未填充的字段保持 nil，由 omitempty 直接省略。
type StatsSummary struct {
	TotalUpload      int64 `json:"totalUpload"`
	TotalDownload    int64 `json:"totalDownload"`
	TotalConnections int64 `json:"totalConnections"`
	TotalDomains     int64 `json:"totalDomains"`
	TotalIPs         int64 `json:"totalIPs"`
	TotalProxies     int64 `json:"totalProxies"`

	TotalRules        *int64 `json:"totalRules,omitempty"`
	TodayUpload       *int64 `json:"todayUpload,omitempty"`
	TodayDownload     *int64 `json:"todayDownload,omitempty"`
	ActiveConnections *int64 `json:"activeConnections,omitempty"`

	TopDomains   []any `json:"topDomains"`
	TopIPs       []any `json:"topIPs"`
	ProxyStats   []any `json:"proxyStats"`
	CountryStats []any `json:"countryStats,omitempty"`
	DeviceStats  []any `json:"deviceStats,omitempty"`
	RuleStats    []any `json:"ruleStats,omitempty"`
	HourlyStats  []any `json:"hourlyStats"`

	DeviceDetailSourceIP *string `json:"deviceDetailSourceIP,omitempty"`
	DeviceDomains        []any   `json:"deviceDomains,omitempty"`
	DeviceIPs            []any   `json:"deviceIPs,omitempty"`

	ProxyDetailChain *string `json:"proxyDetailChain,omitempty"`
	ProxyDomains     []any   `json:"proxyDomains,omitempty"`
	ProxyIPs         []any   `json:"proxyIPs,omitempty"`

	RuleDetailName   *string `json:"ruleDetailName,omitempty"`
	RuleDomains      []any   `json:"ruleDomains,omitempty"`
	RuleIPs          []any   `json:"ruleIPs,omitempty"`
	RuleChainFlowAll any     `json:"ruleChainFlowAll,omitempty"`

	DomainsPage      *PageResult `json:"domainsPage,omitempty"`
	DomainsPageQuery *PageQuery  `json:"domainsPageQuery,omitempty"`
	IPsPage          *PageResult `json:"ipsPage,omitempty"`
	IPsPageQuery     *PageQuery  `json:"ipsPageQuery,omitempty"`

	TrendStats []TrendPoint `json:"trendStats,omitempty"`

	// 内部用于变更检测的计数，不参与序列化
	TopDomainCount int `json:"-"`
	RuleStatCount  int `json:"-"`
}

// PageResult 是分页查询结果。
type PageResult struct {
	Data  []any `json:"data"`
	Total int   `json:"total"`
}

// PageQuery 回显前端发来的分页参数。
type PageQuery struct {
	Offset    int    `json:"offset"`
	Limit     int    `json:"limit"`
	SortBy    string `json:"sortBy,omitempty"`
	SortOrder string `json:"sortOrder,omitempty"`
	Search    string `json:"search,omitempty"`
}

// GeoIPInfo 对齐 shared 的 GeoIPInfo。
type GeoIPInfo struct {
	CountryCode    string `json:"countryCode"`
	CountryName    string `json:"countryName"`
	City           string `json:"city"`
	AsOrganization string `json:"asOrganization"`
}
