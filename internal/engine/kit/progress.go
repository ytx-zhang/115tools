package kit

import (
	"sync/atomic"
	"time"
)

// notifyInterval 进度推送节流间隔（高频计数合并推送）。
const notifyInterval = 200 * time.Millisecond

// Progress 进度计数器：原子 total/completed + 节流通知，外加 running 标志。
// 每个任务单元持一个，SSE 经 onChange 广播。
type Progress struct {
	total     atomic.Int64
	completed atomic.Int64
	running   atomic.Bool

	onChange     func()
	lastNotify   atomic.Int64
	timerPending atomic.Bool
}

// NewProgress 创建进度计数器。onChange 为状态变更回调（可为 nil）。
func NewProgress(onChange func()) *Progress {
	return &Progress{onChange: onChange}
}

// Reset 清零进度（一次批次/执行开始时调用）。
func (p *Progress) Reset() {
	p.total.Store(0)
	p.completed.Store(0)
	p.emit()
}

// AddTotal / AddCompleted 并发上报进度；n<=0 短路。
func (p *Progress) AddTotal(n int64) {
	if n <= 0 {
		return
	}
	p.total.Add(n)
	p.emit()
}

func (p *Progress) AddCompleted(n int64) {
	if n <= 0 {
		return
	}
	p.completed.Add(n)
	p.emit()
}

func (p *Progress) Total() int64     { return p.total.Load() }
func (p *Progress) Completed() int64 { return p.completed.Load() }

// SetRunning 设置 running 标志。
func (p *Progress) SetRunning(v bool) {
	if p.running.Load() != v {
		p.running.Store(v)
		p.emit()
	}
}

// Running 是否运行中。
func (p *Progress) Running() bool { return p.running.Load() }

// emit 节流推送：距上次 ≥notifyInterval 立即推，否则挂一个延迟合并推送。
func (p *Progress) emit() {
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
