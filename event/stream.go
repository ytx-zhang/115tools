// Package event 提供泛型 fan-out 事件流原语，是「组件只经 channel 与外界交互」
// 这一设计模式的标准零件（与 fswatcher.Events() 同款手感）。
//
// 用法：
//
//	src := event.New[T](ring)        // ring 为环形缓冲容量（历史回放用）
//	sub := src.Subscribe(16)          // 订阅者拿到一条独立通道
//	defer src.Unsubscribe(sub)
//	src.Publish(v)                    // 生产者非阻塞广播
//	for ev := range sub { ... }       // 订阅者用 <- 接收
//
// 关键不变量：Publish 永远非阻塞——慢订阅者丢弃最旧事件，绝不拖慢生产者。
package event

import "sync"

// Stream 是泛型 fan-out 事件流：Publish 把事件广播给所有订阅者，
// 同时保留一个固定上限的环形缓冲供新订阅者回放历史。
type Stream[T any] struct {
	mu     sync.RWMutex
	seq    int64
	buf    []elem[T]
	bufCap int
	subs   map[chan T]struct{}
}

type elem[T any] struct {
	Seq int64
	Val T
}

// New 创建事件流，ring 为环形缓冲容量（0 表示不保留历史）。
func New[T any](ring int) *Stream[T] {
	return &Stream[T]{
		bufCap: ring,
		subs:   make(map[chan T]struct{}),
	}
}

// Publish 非阻塞广播一次事件，返回该事件的单调递增序列号。
// 订阅者处理过慢时丢弃本次推送，不影响其他订阅者与生产者。
func (s *Stream[T]) Publish(v T) int64 {
	s.mu.Lock()
	s.seq++
	seq := s.seq
	if s.bufCap > 0 {
		s.buf = append(s.buf, elem[T]{Seq: seq, Val: v})
		if len(s.buf) > s.bufCap {
			s.buf = s.buf[len(s.buf)-s.bufCap:]
		}
	}
	subs := make([]chan T, 0, len(s.subs))
	for ch := range s.subs {
		subs = append(subs, ch)
	}
	s.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- v:
		default: // 慢订阅者：丢弃本次，不拖慢生产者
		}
	}
	return seq
}

// Recent 返回序列号大于 after 的事件（最多 limit 条），用于订阅时历史回放。
func (s *Stream[T]) Recent(after int64, limit int) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, 0, len(s.buf))
	for _, e := range s.buf {
		if e.Seq > after {
			out = append(out, e.Val)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe 返回一个接收新事件的缓冲通道（buf 为缓冲长度）。需配对调用 Unsubscribe。
func (s *Stream[T]) Subscribe(buf int) chan T {
	ch := make(chan T, buf)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	return ch
}

// Unsubscribe 移除订阅者并关闭其通道。
func (s *Stream[T]) Unsubscribe(ch chan T) {
	s.mu.Lock()
	if _, ok := s.subs[ch]; ok {
		delete(s.subs, ch)
		close(ch)
	}
	s.mu.Unlock()
}

// Reset 清空环形缓冲但保留订阅者（供「清空历史」类操作，不切断实时推送）。
func (s *Stream[T]) Reset() {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.mu.Unlock()
}
