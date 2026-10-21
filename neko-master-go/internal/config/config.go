// Package config 集中处理环境变量，命名与上游 collector 保持一致，
// 以便 docker-compose 可以原样复用。
package config

import (
	"os"
	"strconv"
	"strings"
)

// Config 运行期配置。
type Config struct {
	// HTTP
	WebPort string // WEB_PORT，默认 3000（Go 服务自身监听）
	APIPort string // API_PORT，默认 3001
	WSPort  string // COLLECTOR_WS_PORT，默认 3002

	// 存储
	DBPath string // DB_PATH，默认 ./data/stats.db

	// 日志
	LogLevel string // LOG_LEVEL，默认 info

	// GeoIP
	GeoLookupProvider string // GEOIP_LOOKUP_PROVIDER: online | local
	GeoOnlineAPIURL   string // GEOIP_ONLINE_API_URL
	GeoMMDBDir        string // geoip MMDB 所在目录

	// 鉴权
	CookieSecret          string // COOKIE_SECRET
	ForceAccessControlOff bool   // FORCE_ACCESS_CONTROL_OFF
	ShowcaseSiteMode      bool   // SHOWCASE_SITE_MODE

	// 采集调优
	FlushIntervalMS   int // FLUSH_INTERVAL_MS，默认 30000
	FlushMaxBuffer    int // FLUSH_MAX_BUFFER_SIZE，默认 5000
	RealtimeMaxMinute int // REALTIME_MAX_MINUTES，默认 180

	// 后端 TLS
	BackendTLSInsecure bool // BACKEND_TLS_INSECURE=1
}

// Load 从环境变量读取配置。
func Load() *Config {
	c := &Config{
		WebPort: getenv("WEB_PORT", "3000"),
		APIPort: getenv("API_PORT", "3001"),
		WSPort:  getenv("COLLECTOR_WS_PORT", "3002"),
		DBPath:  getenv("DB_PATH", "./data/stats.db"),

		LogLevel: strings.ToLower(getenv("LOG_LEVEL", "info")),

		GeoLookupProvider: getenv("GEOIP_LOOKUP_PROVIDER", "online"),
		GeoOnlineAPIURL:   getenv("GEOIP_ONLINE_API_URL", "https://api.ipinfo.es/ipinfo"),
		GeoMMDBDir:        getenv("GEOIP_MMDB_DIR", "./data/geoip"),

		CookieSecret:          getenv("COOKIE_SECRET", ""),
		ForceAccessControlOff: getenvBool("FORCE_ACCESS_CONTROL_OFF", false),
		ShowcaseSiteMode:      getenvBool("SHOWCASE_SITE_MODE", false),

		FlushIntervalMS:   getenvInt("FLUSH_INTERVAL_MS", 30000),
		FlushMaxBuffer:    getenvInt("FLUSH_MAX_BUFFER_SIZE", 5000),
		RealtimeMaxMinute: getenvInt("REALTIME_MAX_MINUTES", 180),

		BackendTLSInsecure: getenvBool("BACKEND_TLS_INSECURE", false),
	}
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvBool(k string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(k)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}
