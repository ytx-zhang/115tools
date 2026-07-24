package core

import (
	"encoding/json"
	"sync/atomic"
)

// TaskStats 记录一个任务（云端同步 / STRM 生成）的实时进度，
// 并在每次变化时通过 notify 通道知会 web 层，由 SSE 推送到前端面板。
//
// 【并发设计】
//   - total/completed/failed/running 都是原子变量：遍历云端时是几十上百个协程
//     并发累加进度，用原子操作避免加锁开销；
//   - notify 是缓冲为 1 的通道且发送永远非阻塞：业务协程高频更新进度时
//     不会被通知动作拖慢；偶尔丢掉中间信号也没有影响——前端收到任意一次
//     信号后都会重新读取完整快照，最终一定一致。
//
// 【为什么这里没有「失败明细列表」】
// 失败原因不再单独记账（旧设计里 failedErrors 与 slog 重复记录同一条错误）。
// 现在错误只写一次：FailLog 记数 + slog.Error 输出，后者经 logstream 进入
// 前端日志卡片，按「错误」级别过滤即可查看，信息不丢且只有一条链路。
type TaskStats struct {
	total     atomic.Int64  // 需要处理的条目总数
	completed atomic.Int64  // 已完成的条目数
	failed    atomic.Int64  // 失败的条目数（原因见运行日志中的 ERROR 级日志）
	running   atomic.Bool   // 任务是否正在运行（用于防止同一任务被重复启动）
	notify    chan struct{} // 状态变更通知通道（由 Runner 创建、跨热重载实例共享）
}

// NewTaskStats 创建进度统计器。
// notify 由 syncFile.Runner 创建并经根包注入；云端同步与 STRM 生成两个任务
// 共用一个通道，web 层无需区分信号来自哪个任务——收到信号就读取全量快照。
func NewTaskStats(notify chan struct{}) TaskStats {
	return TaskStats{notify: notify}
}

// emitNotify 非阻塞地发送一次「状态有变化」信号。
// 通道已满时直接丢弃本次信号（前端拿到任何一次信号都会读取最新快照，最终一致）。
func (s *TaskStats) emitNotify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// statsSnapshot 是推送给前端的 JSON 结构。
// 注意刻意不包含失败明细：错误日志统一走 slog → logstream 一条管道。
type statsSnapshot struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
	Failed    int64 `json:"failed"`
	Running   bool  `json:"running"`
}

// Reset 在任务开始时清零所有计数。
// running 标记不在此重置，由 TryStart/SetRunning 管理生命周期。
func (s *TaskStats) Reset() {
	s.total.Store(0)
	s.completed.Store(0)
	s.failed.Store(0)
	s.emitNotify()
}

// GetStatus 返回当前进度的 JSON 字符串，供 web 层 SSE 推送给前端。
func (s *TaskStats) GetStatus() string {
	data, err := json.Marshal(statsSnapshot{
		Total:     s.total.Load(),
		Completed: s.completed.Load(),
		Failed:    s.failed.Load(),
		Running:   s.running.Load(),
	})
	if err != nil {
		return "{}"
	}
	return string(data)
}

// TryStart 原子地把 running 从 false 置为 true。
// 返回 false 表示任务已在运行中，调用方应直接放弃本次触发（防重入）。
func (s *TaskStats) TryStart() bool {
	if s.running.CompareAndSwap(false, true) {
		s.emitNotify()
		return true
	}
	return false
}

// SetRunning 直接设置运行标记（任务结束时由 defer 置回 false）。
func (s *TaskStats) SetRunning(v bool) {
	s.running.Store(v)
	s.emitNotify()
}

// Running 返回任务是否正在运行（Stop 方法据此判断是否有任务可停）。
func (s *TaskStats) Running() bool {
	return s.running.Load()
}

// AddTotal 累加待处理总数并通知前端（遍历过程中发现一个新任务就 +1）。
func (s *TaskStats) AddTotal(n int64) { s.total.Add(n); s.emitNotify() }

// AddCompleted 累加完成数并通知前端。
func (s *TaskStats) AddCompleted(n int64) { s.completed.Add(n); s.emitNotify() }

// AddFailed 累加失败数并通知前端。
// 失败原因请同时用 slog.Error 输出（通常由 FailLog 一并完成）。
func (s *TaskStats) AddFailed(n int64) { s.failed.Add(n); s.emitNotify() }

// Total 返回当前任务总数（任务结束时打印汇总日志用）。
func (s *TaskStats) Total() int64 { return s.total.Load() }

// Failed 返回当前失败总数。
// 典型用途：STRM 生成任务结束时判断「零失败才把原始文件移入回收目录」。
func (s *TaskStats) Failed() int64 { return s.failed.Load() }
