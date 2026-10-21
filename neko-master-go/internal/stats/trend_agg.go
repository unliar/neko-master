package stats

import (
	"time"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// TrendAggregated 实现 GET /api/stats/trend/aggregated。
//
// 参数语义（对齐上游 timeseries.repository.ts）：
//   - minutes：回看时长，默认 30，上限 10080
//   - bucketMinutes：桶宽，默认 1，上限 1440
//   - bucketMinutes >= 60 → 从 hourly_stats 聚合；否则从 minute_stats
//   - bucketMinutes >= 1440 且 tzOffset != 0 → 按该时区对齐到「本地日 00:00」
//
// 返回的点按时间升序，时间为 UTC 的 "YYYY-MM-DDTHH:MM:00"。
func (s *Service) TrendAggregated(backendID int64, minutes, bucketMinutes, tzOffsetMinutes int, start, end *time.Time) ([]model.TrendPoint, error) {
	if minutes <= 0 {
		minutes = 30
	}
	if minutes > 60*24*7 {
		minutes = 60 * 24 * 7
	}
	if bucketMinutes <= 0 {
		bucketMinutes = 1
	}
	if bucketMinutes > 24*60 {
		bucketMinutes = 24 * 60
	}
	if tzOffsetMinutes < -840 {
		tzOffsetMinutes = -840
	}
	if tzOffsetMinutes > 840 {
		tzOffsetMinutes = 840
	}

	// 时间窗：优先用显式 start/end，否则按 minutes 回看
	var from, to time.Time
	if start != nil && end != nil {
		from, to = start.UTC(), end.UTC()
	} else {
		to = time.Now().UTC()
		from = to.Add(-time.Duration(minutes) * time.Minute)
	}

	useHourly := bucketMinutes >= 60
	if useHourly {
		return s.aggFromHourly(backendID, from, to, bucketMinutes, tzOffsetMinutes)
	}
	return s.aggFromMinute(backendID, from, to, bucketMinutes)
}

// aggFromMinute 从 minute_stats 聚合。
//
// 桶对齐：把 minute 键按 bucketMinutes 向下取整。
func (s *Service) aggFromMinute(backendID int64, from, to time.Time, bucketMinutes int) ([]model.TrendPoint, error) {
	// 先把起点对齐到桶边界，避免出现半桶
	fromAligned := alignDown(from, bucketMinutes)
	fromKey := fromAligned.Format("2006-01-02T15:04") + ":00"
	toKey := to.Format("2006-01-02T15:04") + ":00"

	rows, err := s.db.SQL().Query(`
		SELECT minute, upload, download FROM minute_stats
		WHERE backend_id = ? AND minute >= ? AND minute <= ?
		ORDER BY minute`, backendID, fromKey, toKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucket struct {
		up, down int64
	}
	buckets := map[time.Time]*bucket{}
	var order []time.Time

	for rows.Next() {
		var minute string
		var up, down int64
		if err := rows.Scan(&minute, &up, &down); err != nil {
			return nil, err
		}
		t, err := time.Parse("2006-01-02T15:04:05", minute)
		if err != nil {
			continue
		}
		key := alignDown(t, bucketMinutes)
		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
			order = append(order, key)
		}
		b.up += up
		b.down += down
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.TrendPoint, 0, len(order))
	for _, k := range order {
		b := buckets[k]
		out = append(out, model.TrendPoint{
			Time:     k.Format("2006-01-02T15:04") + ":00",
			Upload:   b.up,
			Download: b.down,
		})
	}
	return out, nil
}

// aggFromHourly 从 hourly_stats 聚合。
//
// 当 bucketMinutes >= 1440 且 tzOffset != 0 时，按调用方本地日对齐：
// 先把 UTC 小时平移到本地、截断到当天 00:00、再平移回 UTC 输出。
func (s *Service) aggFromHourly(backendID int64, from, to time.Time, bucketMinutes, tzOffsetMinutes int) ([]model.TrendPoint, error) {
	fromAligned := alignDown(toHour(from), 60*24)
	fromKey := fromAligned.Format("2006-01-02T15") + ":00:00"
	toKey := toHour(to).Format("2006-01-02T15") + ":00:00"

	rows, err := s.db.SQL().Query(`
		SELECT hour, upload, download FROM hourly_stats
		WHERE backend_id = ? AND hour >= ? AND hour <= ?
		ORDER BY hour`, backendID, fromKey, toKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucket struct {
		up, down int64
	}
	buckets := map[time.Time]*bucket{}
	var order []time.Time

	offset := time.Duration(tzOffsetMinutes) * time.Minute
	byDay := bucketMinutes >= 1440 && tzOffsetMinutes != 0

	for rows.Next() {
		var hour string
		var up, down int64
		if err := rows.Scan(&hour, &up, &down); err != nil {
			return nil, err
		}
		t, err := time.Parse("2006-01-02T15:04:05", hour)
		if err != nil {
			continue
		}

		var key time.Time
		if byDay {
			// UTC → 本地 → 截断到本地当天 00:00 → 回 UTC
			local := t.Add(offset)
			dayLocal := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
			key = dayLocal.Add(-offset)
		} else {
			key = alignDown(t, bucketMinutes)
		}

		b, ok := buckets[key]
		if !ok {
			b = &bucket{}
			buckets[key] = b
			order = append(order, key)
		}
		b.up += up
		b.down += down
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.TrendPoint, 0, len(order))
	for _, k := range order {
		b := buckets[k]
		out = append(out, model.TrendPoint{
			Time:     k.Format("2006-01-02T15:04") + ":00",
			Upload:   b.up,
			Download: b.down,
		})
	}
	return out, nil
}

// toHour 把时间截断到小时。
func toHour(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location())
}

// alignDown 把时间向下对齐到 bucketMinutes 的整数倍（以 UTC 零点为基准）。
func alignDown(t time.Time, bucketMinutes int) time.Time {
	if bucketMinutes <= 1 {
		return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, time.UTC)
	}
	// 以 Unix 分钟数为基准取整，天然对齐 UTC
	base := t.Unix() / 60
	aligned := (base / int64(bucketMinutes)) * int64(bucketMinutes)
	return time.Unix(aligned*60, 0).UTC()
}
