package core

import "sync/atomic"

// TaskStats 记录一个任务（云端同步 / STRM 生成）的实时进度，
// 并在每次变化时通过 onChange 回调知会 web 层，由 SSE 推送到前端面板。
//
// 【并发设计】
//   - total/completed/running 都是原子变量：遍历云端时是几十上百个协程
//     并发累加进度，用原子操作避免加锁开销；
//   - onChange 由 Runner 提供并负责组装完整快照后 Publish 到事件流；其为非阻塞
//     广播（慢订阅者丢事件），业务协程高频更新进度时不会被通知动作拖慢。
//
// 【状态接口只关心任务进度】
// 失败明细统一走 slog → logstream → 前端日志卡片（按「错误」级别过滤即可查看），
// 状态接口不重复记失败，保持轻量。
type TaskStats struct {
	total     atomic.Int64 // 需要处理的条目总数
	completed atomic.Int64 // 已完成的条目数
	running   atomic.Bool  // 任务是否正在运行（用于防止同一任务被重复启动）
	onChange  func()       // 状态变更回调（由 Runner 注入；为 nil 时不通知）
}

// NewTaskStats 创建进度统计器。
// onChange 由 syncFile.Runner 注入：云端同步与 STRM 生成两个任务共用同一回调，
// 回调内组装完整状态快照并广播——web 层收到事件自带快照，无需回拉。
func NewTaskStats(onChange func()) TaskStats {
	return TaskStats{onChange: onChange}
}

// emitNotify 非阻塞地触发一次「状态有变化」回调（onChange 为 nil 时静默跳过）。
func (s *TaskStats) emitNotify() {
	if s.onChange != nil {
		s.onChange()
	}
}

// TaskProgress 是任务进度的快照，供 web 层 SSE 推送。
type TaskProgress struct {
	Total     int64 `json:"total"`
	Completed int64 `json:"completed"`
	Running   bool  `json:"running"`
}

// Reset 在任务开始时清零所有计数。
// running 标记不在此重置，由 TryStart/SetRunning 管理生命周期。
func (s *TaskStats) Reset() {
	s.total.Store(0)
	s.completed.Store(0)
	s.emitNotify()
}

// Status 返回当前进度快照。
func (s *TaskStats) Status() *TaskProgress {
	return &TaskProgress{
		Total:     s.total.Load(),
		Completed: s.completed.Load(),
		Running:   s.running.Load(),
	}
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

// Total 返回当前任务总数（任务结束时打印汇总日志用）。
func (s *TaskStats) Total() int64 { return s.total.Load() }
