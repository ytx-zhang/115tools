// Package journal 是日志与执行历史中枢。
//
// 它承担三件事：
//   - 执行历史：每次任务执行落一条 Run 记录（触发方式/起止/耗时/统计/结果），持久化到 journal.db；
//   - 执行明细日志：执行过程中产生的逐条日志按 run 归属，运行中在内存实时可查、结束时整批落盘；
//   - 系统程序日志：无任务上下文的全部级别日志落库（上限 maxSystemLogs），经 SSE 推给前端「程序日志」卡片。
//
// 日志唯一入口是 log.go 的 Debug/Info/Warn/Error(ctx, ...)：ctx 携带任务上下文（WithTask 注入）
// 时写入该 run 的明细，否则写入系统程序日志（落库 + SSE 广播）。这与「每个任务卡片看各自日志、
// 系统级消息走程序日志卡片」的产品语义一一对应。
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

// LogEntry 一条日志：一次执行中的明细日志或系统程序日志（复用同构，按存储区分）。
type LogEntry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}
