package store

import (
	"database/sql"
	"strings"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// CountryUpdate 是一条待落库的国家维度增量。
//
// 对齐上游 batch-buffer.ts 的 CountryUpdate：geo 查询结果 + 该 IP 的流量。
type CountryUpdate struct {
	Country     string
	CountryName string
	Continent   string
	Upload      int64
	Download    int64
	Connections int64
	TimestampMs time.Time
}

// geoToCountryUpdate 把 GeoIP 查询结果转成 CountryUpdate。
//
// 空值兜底对齐上游：country/countryName/continent 缺省都是 "Unknown"，
// connections 至少 1。
func geoToCountryUpdate(info *model.GeoIPInfo, up, down, conns int64, ts time.Time) CountryUpdate {
	u := CountryUpdate{Upload: up, Download: down, Connections: conns, TimestampMs: ts}
	if info != nil {
		u.Country = info.CountryCode
		u.CountryName = info.CountryName
		u.Continent = "" // GeoIPInfo 不含 continent，留空由 SQL 兜底成 Unknown
	}
	if u.Country == "" {
		u.Country = "Unknown"
	}
	if u.CountryName == "" {
		u.CountryName = u.Country
	}
	if u.Continent == "" {
		u.Continent = "Unknown"
	}
	if u.Connections < 1 {
		u.Connections = 1
	}
	return u
}

// FlushCountries 把一批国家维度增量写入三张表。
//
// 逐字段对齐上游 country.repository.ts 的 batchUpdateCountryStats：
//   - country_stats          累计（UPSERT 累加，主键 backend_id+country）
//   - minute_country_stats   按 UTC 分钟（主键 backend_id+minute+country）
//   - hourly_country_stats   按 UTC 小时（主键 backend_id+hour+country）
//
// 三张表在同一个事务里，与上游的 this.db.transaction(() => {...}) 一致。
//
// 先在同批次内按 (minute/hour, country) 预聚合成 Map 再写，避免同一批次
// 同一国家被拆成多条 UPSERT（上游同样用 minuteMap/hourlyMap 预聚合）。
func (w *Writer) FlushCountries(backendID int64, updates []CountryUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	tx, err := w.db.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	type bucket struct {
		country, countryName, continent string
		upload, download, conns         int64
	}
	minuteMap := map[string]*bucket{}
	hourMap := map[string]*bucket{}

	for _, u := range updates {
		minute := u.TimestampMs.UTC().Format("2006-01-02T15:04")
		hour := u.TimestampMs.UTC().Format("2006-01-02T15") + ":00:00"

		// ---- country_stats 累计 ----
		if _, err := tx.Exec(`
			INSERT INTO country_stats
				(backend_id, country, country_name, continent, total_upload, total_download, total_connections, last_seen)
			VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(backend_id, country) DO UPDATE SET
				total_upload = total_upload + excluded.total_upload,
				total_download = total_download + excluded.total_download,
				total_connections = total_connections + excluded.total_connections,
				last_seen = CURRENT_TIMESTAMP`,
			backendID, u.Country, u.CountryName, u.Continent,
			u.Upload, u.Download, u.Connections); err != nil {
			return err
		}

		// ---- 预聚合 minute ----
		mk := minute + "\x00" + u.Country
		if b, ok := minuteMap[mk]; ok {
			b.upload += u.Upload
			b.download += u.Download
			b.conns += u.Connections
		} else {
			minuteMap[mk] = &bucket{u.Country, u.CountryName, u.Continent, u.Upload, u.Download, u.Connections}
		}
		// ---- 预聚合 hour ----
		hk := hour + "\x00" + u.Country
		if b, ok := hourMap[hk]; ok {
			b.upload += u.Upload
			b.download += u.Download
			b.conns += u.Connections
		} else {
			hourMap[hk] = &bucket{u.Country, u.CountryName, u.Continent, u.Upload, u.Download, u.Connections}
		}
	}

	for mk, b := range minuteMap {
		minute := mk[:strings.IndexByte(mk, 0)]
		if _, err := tx.Exec(`
			INSERT INTO minute_country_stats
				(backend_id, minute, country, country_name, continent, upload, download, connections)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, minute, country) DO UPDATE SET
				upload = upload + excluded.upload,
				download = download + excluded.download,
				connections = connections + excluded.connections`,
			backendID, minute, b.country, b.countryName, b.continent, b.upload, b.download, b.conns); err != nil {
			return err
		}
	}

	for hk, b := range hourMap {
		hour := hk[:strings.IndexByte(hk, 0)]
		if _, err := tx.Exec(`
			INSERT INTO hourly_country_stats
				(backend_id, hour, country, country_name, continent, upload, download, connections)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(backend_id, hour, country) DO UPDATE SET
				upload = upload + excluded.upload,
				download = download + excluded.download,
				connections = connections + excluded.connections`,
			backendID, hour, b.country, b.countryName, b.continent, b.upload, b.download, b.conns); err != nil {
			return err
		}
	}

	return tx.Commit()
}

var _ = sql.ErrNoRows
