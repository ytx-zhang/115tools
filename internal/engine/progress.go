package engine

import (
	"sync/atomic"
	"time"
)

// notifyInterval 进度推送节流间隔（高频计数合并推送，避免 SSE 刷屏）。
const notifyInterval = 200 * time.Millisecond

// Progress 进度计数器：原子 total/completed + 当前文件 + 运行标志 + 节流通知。
//
// total 在计划阶段一次性确定（由 sync.Applier.Reset 传入），进度条不再边扫边涨地抖动。
type Progress struct {
	total     atomic.Int64
	completed atomic.Int64
	running   atomic.Bool
	current   atomic.Pointer[string]

	onChange     func()
	lastNotify   atomic.Int64
	timerPending atomic.Bool
}

// NewProgress 创建进度计数器。onChange 为状态变更回调（可为 nil）。
func NewProgress(onChange func()) *Progress {
	return &Progress{onChange: onChange}
}

// ──── sync.Progress 接口实现 ────

// Reset 清零并把总数定为 n（一批动作开始时调用，之后 total 不再变化）。
func (p *Progress) Reset(n int64) {
	p.total.Store(n)
	p.completed.Store(0)
	p.emit()
}

// Advance 完成一项。
func (p *Progress) Advance() {
	p.completed.Add(1)
	p.emit()
}

// SetCurrent 记录正在处理的路径（供前端展示「当前文件」）。
func (p *Progress) SetCurrent(path string) {
	if path == "" {
		p.current.Store(nil)
		return
	}
	s := path
	p.current.Store(&s)
	p.emit()
}

// ──── 状态 ────

// SetRunning 设置运行标志。
func (p *Progress) SetRunning(v bool) {
	if p.running.Load() != v {
		p.running.Store(v)
		p.emit()
	}
}

// Snapshot 返回一次性读出的状态快照（避免逐字段读取时的中间态）。
func (p *Progress) Snapshot() (running bool, completed, total int64, current string) {
	running = p.running.Load()
	completed = p.completed.Load()
	total = p.total.Load()
	if s := p.current.Load(); s != nil {
		current = *s
	}
	return running, completed, total, current
}

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
