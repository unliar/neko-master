package clickhouse

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
)

// trafficMinuteCols 是 traffic_detail_buffer / traffic_minute 的列顺序。
var trafficMinuteCols = []string{
	"backend_id", "minute", "domain", "ip",
	"source_ip", "chain", "rule",
	"upload", "download", "connections",
}

var countryCols = []string{
	"backend_id", "minute", "country", "country_name", "continent",
	"upload", "download", "connections",
}

var aggCols = []string{"backend_id", "minute", "upload", "download", "connections"}

// encodeJSONEachRow 把多行编码为 JSONEachRow。
//
// 手写而不是 encoding/json，是为了精确控制类型：
// backend_id 必须是 UInt32 数字，minute 必须是 "YYYY-MM-DD HH:MM:SS" 字符串，
// 流量值必须是 UInt64/UInt32 数字。encoding/json 也能做到，但需要一层反射包装，
// 手写更直接也更快。
func encodeJSONEachRow(table string, rows [][]any) ([]byte, error) {
	var cols []string
	switch table {
	case "traffic_detail_buffer":
		cols = trafficMinuteCols
	case "country_buffer":
		cols = countryCols
	case "traffic_agg_buffer":
		cols = aggCols
	default:
		cols = trafficMinuteCols
	}

	var buf bytes.Buffer
	buf.Grow(len(rows) * (len(cols)*16 + 4))

	for _, r := range rows {
		buf.WriteByte('{')
		for i, c := range cols {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteByte('"')
			buf.WriteString(c)
			buf.WriteString("\":")
			if i < len(r) {
				writeJSONValue(&buf, r[i])
			} else {
				buf.WriteString(`""`)
			}
		}
		buf.WriteString("}\n")
	}
	return buf.Bytes(), nil
}

func writeJSONValue(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		buf.WriteString(`""`)
	case string:
		writeJSONString(buf, x)
	case uint32:
		buf.WriteString(strconv.FormatUint(uint64(x), 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(x, 10))
	case int64:
		if x < 0 {
			buf.WriteByte('0')
			return
		}
		buf.WriteString(strconv.FormatInt(x, 10))
	case int:
		if x < 0 {
			buf.WriteByte('0')
			return
		}
		buf.WriteString(strconv.Itoa(x))
	default:
		writeJSONString(buf, "")
	}
}

func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if c < 0x20 {
				buf.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				buf.WriteByte(hex[c>>4])
				buf.WriteByte(hex[c&0xF])
				continue
			}
			buf.WriteByte(c)
		}
	}
	buf.WriteByte('"')
}

func urlEscape(s string) string { return url.QueryEscape(s) }

func stringsReader(s []byte) *bytes.Reader { return bytes.NewReader(s) }

var _ = strings.TrimSpace
