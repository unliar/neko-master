package model

import "time"

// TrafficUpdate 是一条待写入的流量增量记录。
//
// 放在 model 包，供 collector（生产）与 store（消费）共用，
// 避免 store <-> collector 的循环依赖。
type TrafficUpdate struct {
	TimestampMs time.Time
	Domain      string
	IP          string
	SourceIP    string
	Chain       string // 最终出口
	FullChain   string // "a > b > c"
	Rule        string
	RulePayload string
	Upload      int64
	Download    int64
	Connections int64
}
