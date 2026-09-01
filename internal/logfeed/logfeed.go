// Package logfeed 收集进程内的 Warn/Error 级 slog 日志，供 Web 前端实时展示。
//
// 有界内存环形缓冲（默认 300 条），重启即清空——完整历史由 docker logs 兜底，
// 不落库，符合 v3「日志回归 docker logs、库里不落明细日志」的决策。
package logfeed

import (
	"sync"
	"time"
)

// Entry 一条待展示的日志。JSON 直出，仅透传 slog 已有内容，不含敏感字段。
type Entry struct {
	Seq   uint64    `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"` // "WARN" / "ERROR"
	Msg   string    `json:"msg"`
	Attrs []KV      `json:"attrs,omitempty"`
}

// KV 一条结构化属性（如路径、错误原因），前端以 key=value 形式展示。
type KV struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// defaultCap 缓冲上限（超出后淘汰最旧）。
const defaultCap = 300

// Feed 线程安全的有界环形缓冲 + 非阻塞订阅信号（仿 webui/hub 模式）。
// 定长预分配，物理槽位恒为 seq%cap，布局不变量始终成立。
type Feed struct {
	mu    sync.Mutex
	buf   []Entry
	cap   int
	n     int    // 当前有效条数
	start uint64 // 首条有效 Entry 的 seq（空缓冲时等于 next）
	next  uint64 // 下一条将要分配的 seq，单调递增，清空不复位
	subs  map[chan struct{}]struct{}
}

// NewFeed 创建容量为 cap 的环形缓冲；cap<=0 使用默认 300。
func NewFeed(cap int) *Feed {
	if cap <= 0 {
		cap = defaultCap
	}
	return &Feed{buf: make([]Entry, cap), cap: cap}
}

// Add 环形追加一条日志（seq 由内部分配）并广播「有新日志」信号。
func (f *Feed) Add(e Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e.Seq = f.next
	f.next++
	f.buf[e.Seq%uint64(f.cap)] = e
	if f.n < f.cap {
		f.n++
	} else {
		f.start++
	}
	for ch := range f.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Seq 返回当前最大已分配 seq（即已记录的总条数）。
func (f *Feed) Seq() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.next
}

// entriesLocked 返回按 seq 从旧到新的条目（物理槽位 = seq%cap）。
func (f *Feed) entriesLocked() []Entry {
	out := make([]Entry, 0, f.n)
	for s := f.start; s < f.start+uint64(f.n); s++ {
		out = append(out, f.buf[s%uint64(f.cap)])
	}
	return out
}

// Snapshot 返回全部缓冲条目，最新在前。
func (f *Feed) Snapshot() []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	all := f.entriesLocked()
	out := make([]Entry, len(all))
	for i, e := range all {
		out[len(all)-1-i] = e
	}
	return out
}

// Since 返回 seq 之后（e.Seq>seq）的增量条目，最新在前；
// seq 落后于缓冲起点时返回全部可读条目（部分历史已被淘汰，无法补齐）。
func (f *Feed) Since(seq uint64) []Entry {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == 0 || seq >= f.next {
		return nil
	}
	lo := f.start
	if seq > lo {
		lo = seq + 1
	}
	out := make([]Entry, 0, int(f.next-lo))
	for s := f.next - 1; s >= lo; s-- {
		out = append(out, f.buf[s%uint64(f.cap)])
	}
	return out
}

// Clear 清空缓冲；seq 单调递增不复位，后续增量语义不受影响。
func (f *Feed) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n = 0
	f.start = f.next
}

// Subscribe 订阅「新日志」信号，返回通道与退订函数（慢消费者丢帧，不阻塞生产者）。
func (f *Feed) Subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	f.mu.Lock()
	if f.subs == nil {
		f.subs = make(map[chan struct{}]struct{})
	}
	f.subs[ch] = struct{}{}
	f.mu.Unlock()
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, ch)
		f.mu.Unlock()
	}
}
