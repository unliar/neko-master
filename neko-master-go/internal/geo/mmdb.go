package geo

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/unliar/neko-master/neko-master-go/internal/model"
)

// 以下为最小可用的 MaxMind MMDB 读取器（仅支持 IPv4，只取需要的字段）。
//
// MMDB 结构（v2）：
//   [14 bytes] 分隔符 "\xabMaxMind.com\x00"
//   [4 bytes]  node_count (BigEndian uint32)
//   [2 bytes]  record_size (bits)
//   [4 bytes]  ip_version
//   [2 bytes]  database_type 长度 + N bytes 名称
//   [2 bytes]  language 数量 + N*(len+name)
//   [2 bytes]  description 数量 + ...
//   [4 bytes]  ipv4_node_count
//   [.. ]      tree + data section

const (
	typeExtended   = 0
	typePointer    = 1
	typeUTF8String = 2
	typeDouble     = 3
	typeBytes      = 4
	typeUint16     = 5
	typeUint32     = 6
	typeMap        = 7
	typeInt32      = 8
	typeUint64     = 9
	typeUint128    = 10
	typeSlice      = 11
	typeDataMarker = 12
	typeEndMarker  = 13
	typeBoolean    = 14
	typeFloat      = 15
)

type mmdbMeta struct {
	nodeCount    uint32
	recordSize   int
	ipVersion    uint32
	databaseType string
	ipv4NodeCnt  uint32
	treeStart    int
	dataStart    int
}

type mmdb struct {
	raw  []byte
	meta mmdbMeta
}

// openMMDB 解析 MMDB 元数据。
func openMMDB(raw []byte) (*mmdb, error) {
	if !isMMDB(raw) {
		return nil, fmt.Errorf("not an mmdb file")
	}
	// 分隔符 = 0xab + "MaxMind.com" + 0x00，共 13 字节
	// mmdbMagic 本身不含结尾的 0x00（见 mmdbMagic 定义）
	off := 13
	if off+10 > len(raw) {
		return nil, fmt.Errorf("file too short")
	}

	readU16 := func(o int) uint16 { return binary.BigEndian.Uint16(raw[o : o+2]) }
	readU32 := func(o int) uint32 { return binary.BigEndian.Uint32(raw[o : o+4]) }

	m := &mmdb{raw: raw}
	m.meta.nodeCount = readU32(off)
	m.meta.recordSize = int(readU16(off + 4))
	m.meta.ipVersion = readU32(off + 6)
	off += 10

	// database_type
	tsz := int(readU16(off))
	off += 2
	m.meta.databaseType = string(raw[off : off+tsz])
	off += tsz

	// languages
	ln := int(readU16(off))
	off += 2
	for i := 0; i < ln; i++ {
		lsz := int(readU16(off))
		off += 2 + lsz
	}

	// descriptions
	dn := int(readU16(off))
	off += 2
	for i := 0; i < dn; i++ {
		dsz := int(readU16(off))
		off += 2 + dsz
	}

	// ipv4_node_count
	m.meta.ipv4NodeCnt = readU32(off)
	off += 4

	m.meta.treeStart = off
	// 每个节点含 left/right 两条记录，故节点跨度 = 2 * ceil(recordSize/8)
	nodeBytes := (m.meta.recordSize + 7) / 8
	m.meta.dataStart = off + int(m.meta.nodeCount)*2*nodeBytes

	return m, nil
}

// searchMMDB 按 IPv4 查记录。
//
// MMDB 树语义（关键）：
//   - 每个节点存两个 record 编号：left（bit=0 走）、right（bit=1 走）
//   - record 编号 0 保留，不表示任何节点
//   - record 编号 >= nodeCount 表示「数据段偏移」，即 record = nodeCount + dataIndex
//
// 因此不能按宽度切分记录，必须按 left/right 两半各取一个。
func searchMMDB(raw []byte, ip4 net.IP) (map[string]any, bool) {
	m, err := openMMDB(raw)
	if err != nil {
		return nil, false
	}
	if ip4 == nil || len(ip4) != 4 {
		return nil, false
	}

	recordBits := m.meta.recordSize
	// recBytes = 单条记录的字节数
	recBytes := (recordBits + 7) / 8
	// stride = 一个节点（含 left/right 两条记录）的字节跨度
	stride := 2 * recBytes
	mask := uint32((1 << uint(recordBits)) - 1)

	node := uint32(0)

	for i := 0; i < 32; i++ {
		off := m.meta.treeStart + int(node)*stride
		if off+stride > len(m.raw) {
			return nil, false
		}

		// 节点 = 两个等宽记录，大端
		var left, right uint32
		switch recordBits {
		case 24:
			left = uint32(m.raw[off])<<16 | uint32(m.raw[off+1])<<8 | uint32(m.raw[off+2])
			right = uint32(m.raw[off+3])<<16 | uint32(m.raw[off+4])<<8 | uint32(m.raw[off+5])
		case 28, 32:
			left = binary.BigEndian.Uint32(m.raw[off:off+4]) & mask
			right = binary.BigEndian.Uint32(m.raw[off+4:off+8]) & mask
		default:
			// 通用路径：按字节拼
			for k := 0; k < recBytes; k++ {
				left = (left << 8) | uint32(m.raw[off+k])
				right = (right << 8) | uint32(m.raw[off+recBytes+k])
			}
			left &= mask
			right &= mask
		}

		b := (ip4[i/8] >> (7 - uint(i%8))) & 1
		if b == 0 {
			node = left
		} else {
			node = right
		}
		if node == 0 {
			return nil, false
		}
		if node >= m.meta.nodeCount {
			break // 进入数据区
		}
	}

	if node < m.meta.nodeCount {
		return nil, false
	}

	// MMDB 的 record 编号是「跨全表的记录序号」，不是数据段内的下标。
	// 真实 MMDB 里数据记录紧跟在树后依次排列，record - nodeCount 即为
	// 第几条数据记录；但由于每条数据长度不一，必须靠「前一条解出的
	// 结束偏移」累加定位，不能简单用 dataStart + index。
	//
	// 做法：从 dataStart 开始顺序解码，数到第 (node - nodeCount) 条。
	wantIdx := int(node - m.meta.nodeCount)
	dataOffset := m.meta.dataStart
	for i := 0; i < wantIdx; i++ {
		_, next, ok := m.decodeValue(dataOffset)
		if !ok {
			return nil, false
		}
		dataOffset = next
	}
	if dataOffset >= len(m.raw) {
		return nil, false
	}

	val, _, ok := m.decodeValue(dataOffset)
	if !ok {
		return nil, false
	}
	mp, ok := val.(map[string]any)
	if !ok {
		return nil, false
	}
	return mp, true
}

// decodeMap 解码 map：size 是键值对数量，逐个解码。
func (m *mmdb) decodeMap(off, size int) (any, int, bool) {
	out := map[string]any{}
	p := off
	for i := 0; i < size; i++ {
		k, np1, ok1 := m.decodeValue(p)
		if !ok1 {
			return nil, np1, false
		}
		v, np2, ok2 := m.decodeValue(np1)
		if !ok2 {
			return nil, np2, false
		}
		p = np2
		if ks, isStr := k.(string); isStr {
			out[ks] = v
		}
	}
	return out, p, true
}

// decodeSlice 解码数组：size 是元素数量，逐个解码。
func (m *mmdb) decodeSlice(off, size int) (any, int, bool) {
	out := make([]any, 0, size)
	p := off
	for i := 0; i < size; i++ {
		v, np, ok := m.decodeValue(p)
		if !ok {
			return nil, np, false
		}
		p = np
		out = append(out, v)
	}
	return out, p, true
}

// decodeValue 解析一个 MMDB 数据值，返回 (值, 下一字节偏移, 是否成功)。
func (m *mmdb) decodeValue(off int) (any, int, bool) {
	if off < 0 || off >= len(m.raw) {
		return nil, off, false
	}
	b := m.raw[off]
	off++

	typ := b >> 5
	size := int(b & 0x1F)

	switch typ {
	case typeExtended:
		if off >= len(m.raw) {
			return nil, off, false
		}
		t2 := m.raw[off]
		off++
		typ = t2 + 7
		size = 29
		if off >= len(m.raw) {
			return nil, off, false
		}
		size = 29 + int(m.raw[off])
		off++

	case typePointer:
		// 首字节高 3 位是 001；size 决定指针长度
		var ptr int
		switch size >> 3 & 0x3 {
		case 0: // 3 字节指针
			if off+2 > len(m.raw) {
				return nil, off, false
			}
			ptr = ((int(b) & 0x07) << 16) | (int(m.raw[off]) << 8) | int(m.raw[off+1])
			off += 2
		case 1: // 2 字节指针
			if off+1 > len(m.raw) {
				return nil, off, false
			}
			ptr = ((int(b) & 0x07) << 16) | (int(m.raw[off]) << 8) | int(m.raw[off+1])
			off += 1
		default:
			return nil, off, false
		}
		// 解引用，偏移可能加一个额外 size 字节
		realOff := m.meta.dataStart + ptr
		v, _, ok := m.decodeValue(realOff)
		return v, off, ok

	case typeEndMarker:
		return nil, off, true
	}

	// 变长类型必须在 size 扩展读取之前拦截：
	//   map / slice 的 size 是「对数 / 元素数」，不是字节数。
	// 若继续往下走「body := raw[off:off+size]」，off 只会推进 1+size 字节，
	// 嵌套 map 立刻错位（顶层 key 能解出，第二层 key 就落在字符串中部）。
	switch typ {
	case typeMap:
		return m.decodeMap(off, size)
	case typeSlice:
		return m.decodeSlice(off, size)
	case typeBoolean:
		return size != 0, off, true
	}

	// 读取 size 字节的扩展长度
	if size == 29 {
		if off >= len(m.raw) {
			return nil, off, false
		}
		size = 29 + int(m.raw[off])
		off++
	} else if size == 30 {
		if off+2 > len(m.raw) {
			return nil, off, false
		}
		size = 285 + (int(m.raw[off]) << 8) + int(m.raw[off+1])
		off += 2
	} else if size == 31 {
		if off+3 > len(m.raw) {
			return nil, off, false
		}
		size = 65821 + (int(m.raw[off]) << 16) + (int(m.raw[off+1]) << 8) + int(m.raw[off+2])
		off += 3
	}

	if off+size > len(m.raw) {
		return nil, off, false
	}
	body := m.raw[off : off+size]
	next := off + size

	switch typ {
	case typeUTF8String:
		return string(body), next, true

	case typeDouble:
		if size != 8 {
			return nil, next, false
		}
		var f float64
		bits := binary.BigEndian.Uint64(body)
		// 手工转 float64
		f = float64(bits)
		return f, next, true

	case typeBytes:
		return append([]byte(nil), body...), next, true

	case typeUint16:
		if size != 2 {
			return nil, next, false
		}
		return binary.BigEndian.Uint16(body), next, true

	case typeUint32:
		if size != 4 {
			return nil, next, false
		}
		return binary.BigEndian.Uint32(body), next, true

	case typeUint64:
		if size != 8 {
			return nil, next, false
		}
		return binary.BigEndian.Uint64(body), next, true

	case typeMap:
		out := map[string]any{}
		p := off
		for i := 0; i < size; i++ {
			kv, np, ok := m.decodeValue(p)
			if !ok {
				return nil, next, false
			}
			p = np
			key, ok := kv.(string)
			if !ok {
				// 非字符串 key 也要跳过 value
				_, np2, ok2 := m.decodeValue(p)
				if !ok2 {
					return nil, next, false
				}
				p = np2
				continue
			}
			vv, np3, ok3 := m.decodeValue(p)
			if !ok3 {
				return nil, next, false
			}
			p = np3
			out[key] = vv
		}
		return out, next, true

	case typeInt32:
		if size == 0 {
			return int32(0), next, true
		}
		var v int32
		for _, c := range body {
			v = (v << 8) | int32(c)
		}
		// 符号扩展
		if size < 4 && body[0]&0x80 != 0 {
			v |= int32(-1) << uint(8*size)
		}
		return v, next, true

	case typeBoolean:
		return size != 0, next, true

	case typeFloat:
		if size != 4 {
			return nil, next, false
		}
		bits := binary.BigEndian.Uint32(body)
		return float64(bits), next, true
	}

	return nil, next, false
}

// ---------------------------------------------------------------- 字段抽取

// extractCity 从 GeoLite2-City 记录里抽取国家/城市。
func extractCity(rec map[string]any, info *model.GeoIPInfo) {
	if c, ok := rec["continent"].(map[string]any); ok {
		_ = c
	}
	if c, ok := rec["country"].(map[string]any); ok {
		info.CountryCode = flatStr(c["iso_code"])
		info.CountryName = flatStr(c["names"], "zh-CN")
		if info.CountryName == "" {
			info.CountryName = flatStr(c["names"], "en")
		}
	}
	if c, ok := rec["city"].(map[string]any); ok {
		info.City = flatStr(c["names"], "zh-CN")
		if info.City == "" {
			info.City = flatStr(c["names"], "en")
		}
	}
}

// extractASN 从 GeoLite2-ASN 记录里抽取 ASN/组织名。
func extractASN(rec map[string]any, info *model.GeoIPInfo) {
	org := ""
	if o, ok := rec["autonomous_system_organization"].(string); ok {
		org = o
	}
	num := ""
	if n, ok := rec["autonomous_system_number"]; ok {
		num = numToStr(n)
	}
	if num != "" && org != "" {
		info.AsOrganization = "AS" + num + " " + org
	} else if org != "" {
		info.AsOrganization = org
	} else if num != "" {
		info.AsOrganization = "AS" + num
	}
}

// flatStr 把 MMDB 里可能是 map/string 的值取成字符串。
func flatStr(v any, lang ...string) string {
	switch x := v.(type) {
	case string:
		return x
	case map[string]any:
		for _, l := range lang {
			if s, ok := x[l].(string); ok && s != "" {
				return s
			}
		}
		// 兜底取第一个非空
		for _, val := range x {
			if s, ok := val.(string); ok && s != "" {
				return s
			}
		}
	case uint16:
		return strconv.Itoa(int(x))
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	}
	return ""
}

func numToStr(v any) string {
	switch x := v.(type) {
	case uint16:
		return strconv.Itoa(int(x))
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case int32:
		return strconv.Itoa(int(x))
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}

// 便于调试：把扁平 map 打成点路径
func flatten(m map[string]any) map[string]any {
	out := map[string]any{}
	var walk func(prefix string, v any)
	walk = func(prefix string, v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				walk(prefix+"/"+strings.TrimSpace(k), vv)
			}
		default:
			out[prefix] = v
		}
	}
	walk("", m)
	return out
}
