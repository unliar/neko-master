// Package geo 提供 IP 归属查询（GeoIP / ASN）。
//
// 两条数据源：
//   - online：调用兼容 ipinfo 响应结构的 HTTP API
//   - local：读取本地 MaxMind MMDB（GeoLite2-City / GeoLite2-ASN）
//
// 两级缓存：进程内存 + SQLite 的 geoip_cache / asn_cache 表。
package geo

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

const cacheTTL = 7 * 24 * time.Hour

// mmdbMagic 是 MaxMind DB 的魔数 "\xabMaxMind.com"。
var mmdbMagic = []byte{0xab, 'M', 'a', 'x', 'M', 'i', 'n', 'd', '.', 'c', 'o', 'm'}

// Lookup 是 IP 归属查询器。
type Lookup struct {
	provider string
	apiURL   string
	mmdbDir  string
	db       *sql.DB
	log      *slog.Logger

	http *http.Client

	mu       sync.RWMutex
	memCache map[string]*model.GeoIPInfo

	// 本地 MMDB
	cityPath string
	asnPath  string
	cityRaw  []byte
	asnRaw   []byte
}

// New 创建查询器。provider 为 online / local。
func New(provider, apiURL, mmdbDir string, db *sql.DB, log *slog.Logger) *Lookup {
	if provider != "online" && provider != "local" {
		provider = "online"
	}
	if apiURL == "" {
		apiURL = "https://api.ipinfo.es/ipinfo"
	}
	l := &Lookup{
		provider: provider,
		apiURL:   apiURL,
		mmdbDir:  mmdbDir,
		db:       db,
		log:      log,
		http:     &http.Client{Timeout: 5 * time.Second},
		memCache: make(map[string]*model.GeoIPInfo, 512),
	}
	if provider == "local" {
		l.loadMMDB()
	}
	return l
}

// Provider 返回当前数据源。
func (l *Lookup) Provider() string { return l.provider }

// ---------------------------------------------------------------- 查询

// IP 查询单个 IP 的归属。
// 私有地址与查询失败都返回 nil，绝不阻断主流程。
func (l *Lookup) IP(ip string) *model.GeoIPInfo {
	if ip == "" {
		return nil
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}
	// 内网地址直接放弃，不消耗配额
	if parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
		return nil
	}

	// 内存缓存
	l.mu.RLock()
	if v, ok := l.memCache[ip]; ok {
		l.mu.RUnlock()
		return v
	}
	l.mu.RUnlock()

	// SQLite 缓存
	if v := l.fromDB(ip); v != nil {
		l.mu.Lock()
		l.memCache[ip] = v
		l.mu.Unlock()
		return v
	}

	var v *model.GeoIPInfo
	if l.provider == "local" {
		v = l.lookupLocal(ip)
	} else {
		v = l.lookupOnline(ip)
	}
	if v == nil {
		return nil
	}

	l.mu.Lock()
	l.memCache[ip] = v
	l.mu.Unlock()
	l.saveDB(ip, v)
	return v
}

// Batch 并发查询一批 IP，返回 ip → GeoIPInfo。
// 只对本地查不到的 IP 发请求，且用信号量限流。
func (l *Lookup) Batch(ctx context.Context, ips []string, concurrency int) map[string]*model.GeoIPInfo {
	if len(ips) == 0 {
		return map[string]*model.GeoIPInfo{}
	}
	if concurrency <= 0 {
		concurrency = 8
	}
	if l.provider == "local" && len(l.cityRaw) == 0 {
		return map[string]*model.GeoIPInfo{}
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make(map[string]*model.GeoIPInfo, len(ips))

	for _, ip := range ips {
		wg.Add(1)
		go func(ip string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if v := l.IP(ip); v != nil {
				mu.Lock()
				out[ip] = v
				mu.Unlock()
			}
		}(ip)
	}
	wg.Wait()
	return out
}

// ---------------------------------------------------------------- online

// geoResp 兼容两种上游 schema：
//
//	ip-api.com  : {status, country, countryCode, city, isp, as, query}
//	ipinfo 风格 : {ip, country, city, org, asn}
//
// 两套字段都解析，谁有值用谁，便于换数据源不改代码。
type geoResp struct {
	// 状态字段：ip-api 用 status/error，ipwho.is 用 success
	Status  string `json:"status"`
	Success *bool  `json:"success"`
	Message string `json:"message"`

	// 国家：三套命名都收
	Country     string `json:"country"`      // 全名（三者一致）
	CountryCode string `json:"countryCode"`  // ip-api / ipwho.is
	CC          string `json:"country_code"` // ipwho.is / ip.sb

	City string `json:"city"`

	// 组织名：四套命名都收
	ISP             string `json:"isp"`              // ip-api / ip.sb
	AS              string `json:"as"`               // ip-api "AS15169 Google LLC"
	Org             string `json:"org"`              // ipinfo
	ASNOrganization string `json:"asn_organization"` // ip.sb

	ASN    any    `json:"asn"`          // ip.sb 是数字
	Org2   string `json:"organization"` // ip.sb
	IP     string `json:"ip"`
	Error  string `json:"error"`
	Reason string `json:"reason"` // ipapi.co 的限流提示
}

func (l *Lookup) lookupOnline(ip string) *model.GeoIPInfo {
	url := l.apiURL
	if url != "" && url[len(url)-1] != '/' {
		url += "/"
	}
	url += ip

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	resp, err := l.http.Do(req)
	if err != nil {
		l.log.Debug("geo online lookup failed", "ip", ip, "err", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil
	}
	var r geoResp
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	// 失败语义：ip-api status=fail、ipwho.is success=false、其余 error/reason
	if r.Status == "fail" || (r.Success != nil && !*r.Success) ||
		r.Error != "" || r.Reason != "" {
		l.log.Debug("geo upstream 拒绝", "ip", ip,
			"status", r.Status, "success", r.Success,
			"err", firstNonEmpty(r.Error, r.Reason, r.Message))
		return nil
	}

	org := firstNonEmpty(r.ISP, r.AS, r.Org, r.ASNOrganization, r.Org2)
	code := firstNonEmpty(r.CountryCode, r.CC, r.Country)
	name := firstNonEmpty(r.Country, r.CountryCode, r.CC)

	if code == "" && r.City == "" && org == "" {
		return nil
	}
	return &model.GeoIPInfo{
		CountryCode:    code,
		CountryName:    name,
		City:           r.City,
		AsOrganization: org,
	}
}

// ---------------------------------------------------------------- local

// loadMMDB 加载本地 MMDB 文件。
//
// 文件名固定为 GeoLite2-City.mmdb / GeoLite2-ASN.mmdb（见上游 README 建议）。
func (l *Lookup) loadMMDB() {
	if l.mmdbDir == "" {
		l.log.Warn("geo: local provider 但未配置 mmdb 目录，退化为无归属")
		return
	}
	city := filepath.Join(l.mmdbDir, "GeoLite2-City.mmdb")
	asn := filepath.Join(l.mmdbDir, "GeoLite2-ASN.mmdb")

	if b, err := os.ReadFile(city); err == nil && isMMDB(b) {
		l.cityRaw = b
	} else if err != nil {
		l.log.Warn("geo: 读取 City MMDB 失败", "path", city, "err", err)
	} else {
		l.log.Warn("geo: City MMDB 魔数不匹配", "path", city)
	}

	if b, err := os.ReadFile(asn); err == nil && isMMDB(b) {
		l.asnRaw = b
	} else if err != nil {
		l.log.Warn("geo: 读取 ASN MMDB 失败", "path", asn, "err", err)
	} else {
		l.log.Warn("geo: ASN MMDB 魔数不匹配", "path", asn)
	}

	if len(l.cityRaw) > 0 || len(l.asnRaw) > 0 {
		l.log.Info("geo: 本地 MMDB 已加载",
			"city", len(l.cityRaw) > 0, "asn", len(l.asnRaw) > 0, "dir", l.mmdbDir)
	}
}

func isMMDB(b []byte) bool {
	if len(b) < len(mmdbMagic) {
		return false
	}
	for i, c := range mmdbMagic {
		if b[i] != c {
			return false
		}
	}
	return true
}

// lookupLocal 用自研 MMDB 解析器查归属。
func (l *Lookup) lookupLocal(ip string) *model.GeoIPInfo {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return nil
	}
	ip4 := parsed.To4()
	if ip4 == nil {
		// 只支持 IPv4 的 MMDB 查询
		return nil
	}

	info := &model.GeoIPInfo{}
	got := false

	if len(l.cityRaw) > 0 {
		if rec, ok := searchMMDB(l.cityRaw, ip4); ok {
			got = true
			extractCity(rec, info)
		}
	}
	if len(l.asnRaw) > 0 {
		if rec, ok := searchMMDB(l.asnRaw, ip4); ok {
			got = true
			extractASN(rec, info)
		}
	}

	if !got || (info.CountryCode == "" && info.CountryName == "" && info.City == "" && info.AsOrganization == "") {
		return nil
	}
	return info
}

// ---------------------------------------------------------------- 缓存落库

func (l *Lookup) fromDB(ip string) *model.GeoIPInfo {
	if l.db == nil {
		return nil
	}
	var country, countryName, city, org string
	err := l.db.QueryRow(`
		SELECT COALESCE(country,''), COALESCE(country_name,''),
		       COALESCE(city,''), COALESCE(as_name,'')
		FROM geoip_cache WHERE ip = ?`, ip).
		Scan(&country, &countryName, &city, &org)
	if err != nil {
		return nil
	}
	if country == "" && countryName == "" && city == "" && org == "" {
		return nil
	}
	code := country
	if code == "" {
		code = countryName
	}
	return &model.GeoIPInfo{
		CountryCode:    code,
		CountryName:    firstNonEmpty(countryName, country),
		City:           city,
		AsOrganization: org,
	}
}

func (l *Lookup) saveDB(ip string, v *model.GeoIPInfo) {
	if l.db == nil {
		return
	}
	_, _ = l.db.Exec(`
		INSERT INTO geoip_cache (ip, country, country_name, city, asn, as_name, queried_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(ip) DO UPDATE SET
			country = excluded.country,
			country_name = excluded.country_name,
			city = excluded.city,
			as_name = excluded.as_name,
			queried_at = CURRENT_TIMESTAMP`,
		ip, v.CountryCode, v.CountryName, v.City, "", v.AsOrganization)
}

// ---------------------------------------------------------------- 运维

// Stats 返回 MMDB 加载状态。
func (l *Lookup) Stats() map[string]any {
	return map[string]any{
		"provider":   l.provider,
		"apiUrl":     l.apiURL,
		"mmdbDir":    l.mmdbDir,
		"cityLoaded": len(l.cityRaw) > 0,
		"asnLoaded":  len(l.asnRaw) > 0,
		"memEntries": len(l.memCache),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// 未使用的占位，避免 import 报错
var _ = zip.Reader{}
var _ context.Context
