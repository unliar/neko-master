package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

type fakeGeo struct{ info *model.GeoIPInfo }

func (f fakeGeo) IP(ip string) *model.GeoIPInfo { return f.info }

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	// country_* 三张表有指向 backend_configs 的外键，先塞一个 backend=1
	if _, err := db.sql.Exec(
		`INSERT INTO backend_configs (id, name, url, type) VALUES (1, 't', 'http://127.0.0.1:1', 'clash')`,
	); err != nil {
		t.Fatalf("seed backend: %v", err)
	}
	return db
}

// geoToCountryUpdate \u7684\u7a7a\u503c\u515c\u5e95\u5e94\u5168\u90e8\u843d\u5230 Unknown / connections>=1\u3002
func TestGeoToCountryUpdateDefaults(t *testing.T) {
	ts := time.Unix(1760000000, 0).UTC()
	u := geoToCountryUpdate(nil, 100, 200, 0, ts)
	if u.Country != "Unknown" || u.CountryName != "Unknown" || u.Continent != "Unknown" {
		t.Errorf("nil geo \u5e94\u5168 Unknown\uff0c\u5f97\u5230 %+v", u)
	}
	if u.Connections != 1 {
		t.Errorf("connections \u5e94\u63d0\u5347\u5230 1\uff0c\u5f97\u5230 %d", u.Connections)
	}

	info := &model.GeoIPInfo{CountryCode: "US", CountryName: "United States"}
	u2 := geoToCountryUpdate(info, 1, 2, 5, ts)
	if u2.Country != "US" || u2.CountryName != "United States" {
		t.Errorf("geo \u5e94\u88ab\u91c7\u7eb3\uff0c\u5f97\u5230 %+v", u2)
	}
	if u2.Continent != "Unknown" {
		t.Errorf("GeoIPInfo \u65e0 continent\uff0c\u5e94\u515c\u5e95 Unknown\uff0c\u5f97\u5230 %q", u2.Continent)
	}
}

// \u4e09\u5f20\u8868\u90fd\u8981\u88ab\u5199\u5165\uff0c\u4e14\u91cd\u590d\u8c03\u7528\u5e94\u7d2f\u52a0\uff08UPSERT\uff09\u3002
func TestFlushCountriesWritesThreeTables(t *testing.T) {
	db := openTestDB(t)
	w := NewWriter(db)
	ts := time.Unix(1760000000, 0).UTC()

	batch := []CountryUpdate{
		{Country: "US", CountryName: "United States", Continent: "Unknown", Upload: 100, Download: 200, Connections: 3, TimestampMs: ts},
	}
	if err := w.FlushCountries(1, batch); err != nil {
		t.Fatalf("flush1: %v", err)
	}
	// \u540c\u4e00\u56fd\u5bb6\u518d\u5199\u4e00\u6b21\uff0c\u5e94\u7d2f\u52a0
	if err := w.FlushCountries(1, []CountryUpdate{
		{Country: "US", CountryName: "United States", Continent: "Unknown", Upload: 10, Download: 20, Connections: 2, TimestampMs: ts},
	}); err != nil {
		t.Fatalf("flush2: %v", err)
	}

	var up, down, conns int64
	if err := db.sql.QueryRow(
		`SELECT total_upload,total_download,total_connections FROM country_stats WHERE backend_id=1 AND country='US'`,
	).Scan(&up, &down, &conns); err != nil {
		t.Fatalf("read country_stats: %v", err)
	}
	if up != 110 || down != 220 || conns != 5 {
		t.Errorf("country_stats \u7d2f\u52a0\u9519\u8bef: %d/%d/%d\uff0c\u5e94\u4e3a 110/220/5", up, down, conns)
	}

	var mn, mc int64
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM minute_country_stats WHERE backend_id=1 AND country='US'`).Scan(&mn)
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM hourly_country_stats WHERE backend_id=1 AND country='US'`).Scan(&mc)
	if mn != 1 || mc != 1 {
		t.Errorf("minute/hourly \u884c\u6570\u5e94\u5404\u4e3a 1\uff08\u540c\u6876\u5408\u5e76\uff09\uff0c\u5f97\u5230 min=%d hour=%d", mn, mc)
	}
}

// \u540c\u4e00\u5206\u949f\u540c\u56fd\u5bb6\u591a\u6761\u5e94\u5408\u5e76\u6210\u4e00\u884c\u3002
func TestFlushCountriesMergesSameMinute(t *testing.T) {
	db := openTestDB(t)
	w := NewWriter(db)
	ts := time.Unix(1760000000, 0).UTC()

	if err := w.FlushCountries(1, []CountryUpdate{
		{Country: "SG", CountryName: "Singapore", Continent: "Asia", Upload: 1, Download: 1, Connections: 1, TimestampMs: ts},
		{Country: "SG", CountryName: "Singapore", Continent: "Asia", Upload: 2, Download: 2, Connections: 1, TimestampMs: ts.Add(time.Second)},
	}); err != nil {
		t.Fatalf("flush: %v", err)
	}

	var n, up int64
	_ = db.sql.QueryRow(`SELECT COUNT(*), COALESCE(SUM(upload),0) FROM minute_country_stats WHERE backend_id=1 AND country='SG'`).Scan(&n, &up)
	if n != 1 || up != 3 {
		t.Errorf("\u540c\u5206\u949f\u5408\u5e76\u5931\u8d25: rows=%d upload=%d\uff0c\u5e94\u4e3a 1/3", n, up)
	}
}

// writeCountries\uff1a\u6309 IP \u67e5 geo\uff0c\u65e0 geo \u7684 IP\uff08\u5074\u63a5 fakeGeo \u8fd4\u56de nil\uff09\u4e0d\u843d\u5e93\u3002
func TestWriteCountriesSkipsNoGeo(t *testing.T) {
	db := openTestDB(t)
	w := NewWriter(db)
	w.AttachGeo(fakeGeo{info: nil}) // \u6240\u6709 IP \u90fd\u67e5\u4e0d\u5230

	ts := time.Unix(1760000000, 0).UTC()
	err := w.writeCountries(1, []model.TrafficUpdate{
		{IP: "8.8.8.8", Upload: 100, Download: 100, Connections: 1, TimestampMs: ts},
	})
	if err != nil {
		t.Fatalf("writeCountries: %v", err)
	}
	var n int64
	_ = db.sql.QueryRow(`SELECT COUNT(*) FROM country_stats WHERE backend_id=1`).Scan(&n)
	if n != 0 {
		t.Errorf("\u65e0 geo \u65f6\u4e0d\u5e94\u843d\u5e93\uff0c\u5f97\u5230 %d \u884c", n)
	}
}

// writeCountries\uff1a\u6709 geo \u65f6\u6309 IP \u5408\u5e76\u6d41\u91cf\u3002
func TestWriteCountriesAggregatesByIP(t *testing.T) {
	db := openTestDB(t)
	w := NewWriter(db)
	w.AttachGeo(fakeGeo{info: &model.GeoIPInfo{CountryCode: "JP", CountryName: "Japan"}})

	ts := time.Unix(1760000000, 0).UTC()
	err := w.writeCountries(1, []model.TrafficUpdate{
		{IP: "1.1.1.1", Upload: 100, Download: 0, Connections: 1, TimestampMs: ts},
		{IP: "1.1.1.1", Upload: 50, Download: 0, Connections: 0, TimestampMs: ts.Add(time.Second)},
		{IP: "", Upload: 999, Download: 999, Connections: 9, TimestampMs: ts}, // \u7a7a IP \u8df3\u8fc7
	})
	if err != nil {
		t.Fatalf("writeCountries: %v", err)
	}

	var up int64
	_ = db.sql.QueryRow(`SELECT total_upload FROM country_stats WHERE backend_id=1 AND country='JP'`).Scan(&up)
	if up != 150 {
		t.Errorf("IP \u6d41\u91cf\u5e94\u5408\u5e76\u4e3a 150\uff0c\u5f97\u5230 %d", up)
	}
}

// geo \u672a\u6302\u8f7d\uff08nil\uff09\u65f6 writeCountries \u4e0d\u4f1a\u88ab\u8c03\u7528\uff08\u7531 Flush \u95ed\u5173\uff09\uff0c
// \u8fd9\u91cc\u76f4\u63a5\u9a8c\u8bc1 FlushCountries \u5bf9\u7a7a\u8f93\u5165 no-op\u3002
func TestFlushCountriesEmpty(t *testing.T) {
	db := openTestDB(t)
	w := NewWriter(db)
	if err := w.FlushCountries(1, nil); err != nil {
		t.Errorf("\u7a7a\u8f93\u5165\u5e94 no-op\uff0c\u5f97\u5230 %v", err)
	}
}
