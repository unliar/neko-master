package model

// Target 描述一个采集目标（backend 的采集视图）。
//
// 放在 model 包以避免 store <-> collector 的循环依赖。
type Target struct {
	ID    int64
	Name  string
	URL   string
	Token string
	Type  string // clash | surge | agent
}
