// Package push 实现「本地 → 云端」同步：文件监听、全量扫描、上传、云端清理。
package push

import (
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/journal"
)

// DirPool 目录池（生产者-消费者）：chan 排队 + pending 去重（已投未消费则合并，手动来源升级）。
type DirPool struct {
	ch      chan string
	mu      sync.Mutex
	pending map[string]journal.Trigger
}

// NewDirPool 创建目录池。
func NewDirPool() *DirPool {
	return &DirPool{ch: make(chan string, 64), pending: make(map[string]journal.Trigger)}
}

// Enqueue 投递目录：已投未消费则合并，manual 来源升级（保证全量扫描显示 INFO）。
func (p *DirPool) Enqueue(dir string, trigger journal.Trigger) {
	p.mu.Lock()
	prev, loaded := p.pending[dir]
	if loaded {
		if trigger == journal.TriggerManual && prev != journal.TriggerManual {
			p.pending[dir] = journal.TriggerManual
		}
		p.mu.Unlock()
		return
	}
	p.pending[dir] = trigger
	p.mu.Unlock()
	p.ch <- dir
}

// Chan 返回待处理目录通道（只读），供消费者循环 select。
func (p *DirPool) Chan() <-chan string { return p.ch }

// Take 取出一个目录并返回其最终触发来源（消费时调用，与写入互斥）。
func (p *DirPool) Take(dir string) journal.Trigger {
	p.mu.Lock()
	defer p.mu.Unlock()
	tr := p.pending[dir]
	delete(p.pending, dir)
	return tr
}

// Clear 丢弃未处理目录（停止 push 时调用）：清空 pending + 非阻塞排空 chan。
func (p *DirPool) Clear() {
	p.mu.Lock()
	clear(p.pending)
	p.mu.Unlock()
	for {
		select {
		case <-p.ch:
		default:
			return
		}
	}
}

// ──── 非视频事件的防抖合批器 ────

// dirBatcher 非视频事件（目录/.strm 增删改）的防抖合批器：
// Arm 登记父目录并重置防抖定时；窗口内无新事件才到点唤醒消费者一次性 Take 整批投池。
type dirBatcher struct {
	mu      sync.Mutex
	pending map[string]struct{}
	kick    chan struct{}
	timer   *time.Timer
	window  func() time.Duration
}

func newDirBatcher(window func() time.Duration) *dirBatcher {
	return &dirBatcher{pending: make(map[string]struct{}), kick: make(chan struct{}, 1), window: window}
}

func (b *dirBatcher) notify() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Arm 登记一个目录并重置防抖定时（首次惰性创建，后续 Reset）。
func (b *dirBatcher) Arm(dir string) {
	b.mu.Lock()
	b.pending[dir] = struct{}{}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.window(), b.notify)
	} else {
		b.timer.Reset(b.window())
	}
	b.mu.Unlock()
}

// Take 取出并清空合集。
func (b *dirBatcher) Take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := slices.Collect(maps.Keys(b.pending))
	clear(b.pending)
	return items
}

// Kick 返回防抖到点唤醒通道（只读）。
func (b *dirBatcher) Kick() <-chan struct{} { return b.kick }

// Stop 停止防抖定时。
func (b *dirBatcher) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}
