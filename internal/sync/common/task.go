package common

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/status"
)

// notifyInterval 进度通知节流间隔：AddTotal/AddCompleted 高频调用（大库上传成千上万次），
// 逐次推 SSE 会把日志流撑爆。节流合并为「至少间隔 notifyInterval 推一次最新状态」。
const notifyInterval = 200 * time.Millisecond

// progress 进度计数器：原子 total/completed + 节流通知。
// 与运行/取消机制（Task）解耦，仅负责「已处理多少 / 总共多少」的统计与上报，
// 不含任何 running/stop 状态——那是 Task 的职责。
// 节流无锁化：lastNotify 用原子时间戳做「立即推送」判定（CAS 去重），
// timerPending 用原子布尔保证「延迟推送 timer 只挂一个」。
type progress struct {
	total     atomic.Int64
	completed atomic.Int64

	onChange     func()
	lastNotify   atomic.Int64 // 上次推送时刻（UnixNano）
	timerPending atomic.Bool  // 是否已挂延迟推送 timer
}

// Reset 清零进度（run 业务体开始时调用）。
func (p *progress) Reset() {
	p.total.Store(0)
	p.completed.Store(0)
	p.emitNotify()
}

// AddTotal / AddCompleted 并发上报进度。
// 通用约定（三个任务统一）：调用方只在确认有实际待处理项时调用，一次调用 = 一项；
// 成功处理后才 AddCompleted，失败只加 total 不加 completed（进度条显示差额）。
// n<=0 直接短路是最后防线：调用点漏判 0 时（如已同步文件的 len(up)==0）也不产生
// 内容相同的无效状态帧，避免大库任务把 SSE 日志流撑爆。
func (p *progress) AddTotal(n int64) {
	if n <= 0 {
		return
	}
	p.total.Add(n)
	p.emitNotify()
}

func (p *progress) AddCompleted(n int64) {
	if n <= 0 {
		return
	}
	p.completed.Add(n)
	p.emitNotify()
}

func (p *progress) Total() int64 { return p.total.Load() }

// Completed 返回已完成数（与 Total 相等表示上一批次已结束）。
func (p *progress) Completed() int64 { return p.completed.Load() }

// emitNotify 节流推送：距上次推送 ≥notifyInterval 立即推（CAS 抢到一次推送权）；
// 否则保证「延迟推送 timer 至多挂一个」，间隔后推一次合并此间全部进度变化。
// onChange 在无锁路径下调用；多一次/早一点推送无害（节流只合并高频，不保证恰好一次）。
func (p *progress) emitNotify() {
	if p.onChange == nil {
		return
	}
	interval := int64(notifyInterval)
	now := time.Now().UnixNano()
	if last := p.lastNotify.Load(); now-last >= interval && p.lastNotify.CompareAndSwap(last, now) {
		p.onChange()
		return
	}
	if !p.timerPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(notifyInterval, func() {
		p.timerPending.Store(false)
		p.lastNotify.Store(time.Now().UnixNano())
		p.onChange()
	})
}

// Task 一次性任务：防重入启动 + 可取消上下文 + running 标志。
// 进度统计委托给内嵌的 progress（见上），本结构只管「运行态」与取消。
// 云端同步与 STRM 生成各持一个，各自只写 Run(ctx) 业务体即可。
// running 为原子量；进度变化经节流后的 onChange 通知 web 层 SSE。
type Task struct {
	progress *progress
	name     string
	running  atomic.Bool
	cancel   context.CancelCauseFunc
}

// NewTask 创建一次性任务。
func NewTask(name string, onChange func()) *Task {
	return &Task{name: name, progress: &progress{onChange: onChange}}
}

// Start 启动任务：已在运行直接返回 false（CAS 防重入）；否则构造可取消 ctx
// 异步执行业务体，结束后（正常或取消）统一收尾——发结束日志 + 置 running=false。
func (t *Task) Start(ctx context.Context, run func(context.Context)) bool {
	if !t.running.CompareAndSwap(false, true) {
		return false
	}
	t.progress.emitNotify()
	ctx, t.cancel = context.WithCancelCause(ctx)
	go func() {
		defer func() {
			t.running.Store(false)
			t.progress.emitNotify()
		}()
		run(ctx)
	}()
	return true
}

// Stop 取消任务（若运行中）。永远成功返回。
func (t *Task) Stop() {
	if t.running.Load() && t.cancel != nil {
		t.cancel(nil)
	}
}

// SetRunning 显式设置 running 标志（供常驻消费者循环驱动：取到目录=亮、空闲=灭）。
// 与 Start 的 CAS 防重入不同：消费者循环常驻，running 仅表示「当前是否有目录在处理」。
func (t *Task) SetRunning(v bool) {
	if t.running.Load() != v {
		t.running.Store(v)
		t.progress.emitNotify()
	}
}

// Status 返回任务进度快照。
func (t *Task) Status() *status.TaskStatus {
	return &status.TaskStatus{
		Total:     t.progress.Total(),
		Completed: t.progress.Completed(),
		Running:   t.running.Load(),
	}
}

// Reset 清零进度（委托 progress）。run 业务体开始时调用。
func (t *Task) Reset() { t.progress.Reset() }

// AddTotal / AddCompleted 并发上报进度（委托 progress）。
func (t *Task) AddTotal(n int64) { t.progress.AddTotal(n) }

func (t *Task) AddCompleted(n int64) { t.progress.AddCompleted(n) }

func (t *Task) Total() int64 { return t.progress.Total() }
