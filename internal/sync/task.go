package sync

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/logs"
	"sync/atomic"
)

// TaskProgress 是任务进度的快照（类型别名，零开销；JSON 字段勿动，前端依赖）。
type TaskProgress = logs.TaskStatus

// Task 一次性任务：防重入启动 + 可取消上下文 + 原子进度上报一体。
// 云端同步与 STRM 生成各持一个，各自只写 run(ctx) 业务体即可。
//
// total/completed/running 均为原子量——遍历云端时几十上百协程并发累加进度，
// 用原子操作避免加锁；进度每次变化经 onChange 回调（Syncer 注入，非阻塞广播）
// 知会 web 层 SSE。
type Task struct {
	name      string
	onChange  func()
	total     atomic.Int64
	completed atomic.Int64
	running   atomic.Bool
	cancel    context.CancelCauseFunc
}

func NewTask(name string, onChange func()) *Task {
	return &Task{name: name, onChange: onChange}
}

// Start 启动任务：已在运行直接返回 false（CAS 防重入）；否则构造可取消 ctx
// 异步执行业务体，结束后（正常或取消）统一收尾——发结束日志 + 置 running=false。
func (t *Task) Start(ctx context.Context, run func(context.Context)) bool {
	if !t.running.CompareAndSwap(false, true) {
		return false
	}
	t.emitNotify()
	ctx, t.cancel = context.WithCancelCause(ctx)
	go func() {
		defer func() {
			t.running.Store(false)
			t.emitNotify()
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

// Status 返回任务进度快照。
func (t *Task) Status() *TaskProgress {
	return &TaskProgress{
		Total:     t.total.Load(),
		Completed: t.completed.Load(),
		Running:   t.running.Load(),
	}
}

// Reset 清零进度（run 业务体开始时调用）。
func (t *Task) Reset() {
	t.total.Store(0)
	t.completed.Store(0)
	t.emitNotify()
}

// AddTotal / AddCompleted 供 run 业务体并发上报进度。
func (t *Task) AddTotal(n int64)     { t.total.Add(n); t.emitNotify() }
func (t *Task) AddCompleted(n int64) { t.completed.Add(n); t.emitNotify() }
func (t *Task) Total() int64         { return t.total.Load() }

func (t *Task) emitNotify() {
	if t.onChange != nil {
		t.onChange()
	}
}
