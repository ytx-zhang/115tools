package webui

import "sync"

// Hub 是轻量状态变更广播：引擎进度变化时 publish，SSE 订阅者据此推最新状态。
// 非阻塞广播，慢订阅者丢弃本次信号（下一次变更仍会收到），不阻塞生产者。
type Hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

// NewHub 创建广播中心。
func NewHub() *Hub {
	return &Hub{subs: make(map[chan struct{}]struct{})}
}

// Publish 非阻塞广播一次状态变更信号。
func (h *Hub) Publish() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Subscribe 订阅状态变更信号，返回通道与取消函数。
func (h *Hub) Subscribe() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}
