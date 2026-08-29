// Package journal 是全新的日志与执行历史中枢，替代旧项目的全局日志流。
//
// 它承担三件事：
//   - 执行历史：每次任务执行落一条 Run 记录（触发方式/起止/耗时/统计/结果），持久化到 journal.db；
//   - 执行明细日志：执行过程中产生的逐条日志按 run 归属，运行中在内存实时可查、结束时整批落盘；
//   - 系统横幅：无任务上下文的 Warn/Error 记录进横幅环，经 SSE 推给前端在任务中心顶部展示。
//
// 日志唯一入口是 api.go 的 Debug/Info/Warn/Error(ctx, ...)：ctx 携带任务上下文（WithTask 注入）
// 时写入该 run 的明细，否则仅终端输出（≥Warn 同时进横幅）。这与「每个任务卡片看各自日志、
// 系统级消息走横幅」的产品语义一一对应。
package journal

import "time"

// State 一次执行的状态。
type State string

const (
	StateRunning  State = "running"
	StateSuccess  State = "success"
	StateCanceled State = "canceled"
	StateFailed   State = "failed"
)

// Trigger 一次执行的触发方式。
type Trigger string

const (
	TriggerManual Trigger = "manual" // 手动点击执行
	TriggerCron   Trigger = "cron"   // 定时触发
	TriggerWatch  Trigger = "watch"  // 文件事件监听触发
	TriggerInit   Trigger = "init"   // 启动初始化触发
)

// Direction 一次执行的方向。
type Direction string

const (
	DirPush Direction = "push" // 本地 → 云端
	DirPull Direction = "pull" // 云端 → 本地
)

// Counters 一次执行的统计计数。
type Counters struct {
	Scanned       int64 `json:"scanned"`
	Uploaded      int64 `json:"uploaded"`
	Downloaded    int64 `json:"downloaded"`
	StrmGenerated int64 `json:"strm_generated"`
	Deleted       int64 `json:"deleted"`
	Skipped       int64 `json:"skipped"`
	Failed        int64 `json:"failed"`
}

// Run 一次任务执行的记录。
type Run struct {
	Seq        uint64    `json:"seq"`
	TaskID     string    `json:"task_id"`
	TaskName   string    `json:"task_name"`
	Direction  Direction `json:"direction"`
	Trigger    Trigger   `json:"trigger"`
	State      State     `json:"state"`
	StartedAt  time.Time `json:"started_at"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	Counters   Counters  `json:"counters"`
	Error      string    `json:"error,omitempty"`
}

// LogEntry 一次执行中的一条明细日志。
type LogEntry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Banner 系统级横幅（无任务上下文的 Warn/Error）。
type Banner struct {
	Level   string    `json:"level"`
	Msg     string    `json:"msg"`
	Attrs   string    `json:"attrs,omitempty"`
	Time    time.Time `json:"time"`
	Cleared bool      `json:"cleared,omitempty"` // 清空信号：仅广播不入环，SSE 客户端据此清空本地横幅
}
