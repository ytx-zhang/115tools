// Package status 定义任务状态快照类型（TaskStatus / StatusData），供 web 层 SSE 与 sync 层共用。
//
// 设计为「叶子包」：不依赖 logs / drive / config 等任何可能反向依赖本包的模块，
// 因此既能被 logs（Entry.Status / LogStatus）引用，也能被 sync/common（Task.Status）引用，
// 而不会产生 import 循环（common 经 drive/config 间接依赖 logs，故 logs 不能直接 import common）。
//
// ⚠️ 前端依赖以下 JSON 字段名，改动必须同步前端。
package status

// TaskStatus 单任务进度快照（供 web 层 SSE 消费）。
type TaskStatus struct {
	Running   bool  `json:"running"`
	Completed int64 `json:"completed"`
	Total     int64 `json:"total"`
}

// StatusData 推送前端的完整任务状态快照。
// ⚠️ 前端依赖以下 JSON 字段名，改动必须同步前端：config_ready/missing/init_error/sync/strm/local。
type StatusData struct {
	ConfigReady bool        `json:"config_ready"`
	Missing     []string    `json:"missing,omitempty"`
	InitError   string      `json:"init_error,omitempty"`
	Sync        *TaskStatus `json:"sync"`
	Strm        *TaskStatus `json:"strm"`
	Local       *TaskStatus `json:"local"`
}
