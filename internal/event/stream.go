// Package event 提供泛型 fan-out 事件流原语，是「组件只经 channel 与外界交互」
// 这一设计模式的标准零件（与 fswatcher.Events() 同款手感）。同时内置一个
// 日志广播特化层：把全局 slog 输出捕获进内存环形缓冲，供管理面板经 SSE 实时展示。
//
// 事件流用法：
//
//	src := event.New[T](ring)        // ring 为环形缓冲容量（历史回放用）
//	sub := src.Subscribe(16)          // 订阅者拿到一条独立通道
//	defer src.Unsubscribe(sub)
//	src.Publish(v)                    // 生产者非阻塞广播
//	for ev := range sub { ... }       // 订阅者用 <- 接收
//
// 关键不变量：Publish 永远非阻塞——慢订阅者丢弃最旧事件，绝不拖慢生产者。
package event

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Setup 配置全局 slog：解析 LOG_LEVEL 环境变量得到输出级别，
// 用带毫秒时间格式的 TextHandler 输出到 stdout，
// 再包裹一层捕获 handler 把每条日志同步送进 hub（前端「日志」卡片的数据源）。
//
// 调用方：main 启动时调用一次。自此程序里所有 slog.Xxx 调用都会
// 「一份日志，两处消费」：终端可见，面板实时可见。
func Setup(hub *Hub) {
	levelStr := strings.ToUpper(os.Getenv("LOG_LEVEL"))
	var level slog.Level

	switch levelStr {
	case "DEBUG":
		level = slog.LevelDebug
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05.000"))
			}
			return a
		},
	}
	slog.SetDefault(slog.New(WrapHandler(slog.NewTextHandler(os.Stdout, opts), hub)))
}

const (
	// ringSize 内存中保留的最近日志条数。
	ringSize = 1000
	// subBuf 每个 SSE 订阅者的发送缓冲，满则丢弃最旧（慢客户端不拖垮日志写入）。
	subBuf = 128
)

// Entry 一条结构化日志的快照。
type Entry struct {
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Hub 保存近期日志并向订阅者广播，底层基于 Stream 事件流。
type Hub struct {
	stream *Stream[Entry]
}

// NewHub 创建空 Hub。
func NewHub() *Hub {
	return &Hub{stream: New[Entry](ringSize)}
}

// Write 追加一条日志并广播给订阅者。并发安全。
func (h *Hub) Write(e Entry) {
	h.stream.Publish(e)
}

// Recent 返回最近最多 limit 条日志（limit<=0 表示全部），用于订阅时的历史回放。
func (h *Hub) Recent(limit int) []Entry {
	return h.stream.Recent(limit)
}

// Subscribe 返回一个接收新日志的通道，需配对调用 Unsubscribe。
func (h *Hub) Subscribe() chan Entry { return h.stream.Subscribe(subBuf) }

// Unsubscribe 移除订阅者并关闭通道。
func (h *Hub) Unsubscribe(ch chan Entry) { h.stream.Unsubscribe(ch) }

// Clear 清空内存中的日志缓冲（不影响正在进行的实时推送）。
func (h *Hub) Clear() {
	h.stream.Reset()
}

// captureHandler 包裹任意 slog.Handler：先交由原 handler 输出（如 stdout），
// 再把记录转发给 Hub，实现「一份日志，两处消费」。
type captureHandler struct {
	slog.Handler
	hub *Hub
}

// WrapHandler 用捕获层包裹 handler，使日志同时进入指定的 event Hub。
// hub 由调用方创建并显式注入（不再使用全局默认 Hub）。
func WrapHandler(h slog.Handler, hub *Hub) slog.Handler {
	return &captureHandler{Handler: h, hub: hub}
}

func (c *captureHandler) Handle(ctx context.Context, r slog.Record) error {
	_ = c.Handler.Handle(ctx, r)

	var sb strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if sb.Len() > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(a.Key)
		sb.WriteByte('=')
		sb.WriteString(a.Value.String())
		return true
	})

	c.hub.Write(Entry{
		Time:  r.Time,
		Level: r.Level.String(),
		Msg:   r.Message,
		Attrs: sb.String(),
	})
	return nil
}

// WithAttrs 透传到内层 handler，返回新的捕获层。
func (c *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{Handler: c.Handler.WithAttrs(attrs), hub: c.hub}
}

// WithGroup 透传到内层 handler，返回新的捕获层。
func (c *captureHandler) WithGroup(name string) slog.Handler {
	return &captureHandler{Handler: c.Handler.WithGroup(name), hub: c.hub}
}

// Stream 是泛型 fan-out 事件流：Publish 把事件广播给所有订阅者，
// 同时保留一个固定上限的环形缓冲供新订阅者回放历史。
type Stream[T any] struct {
	mu     sync.RWMutex
	buf    []T
	bufCap int
	subs   map[chan T]struct{}
}

// New 创建事件流，ring 为环形缓冲容量（0 表示不保留历史）。
func New[T any](ring int) *Stream[T] {
	return &Stream[T]{
		bufCap: ring,
		subs:   make(map[chan T]struct{}),
	}
}

// Publish 非阻塞广播一次事件，同时压入环形缓冲（历史回放用）。
// 订阅者处理过慢时丢弃本次推送，不影响其他订阅者与生产者。
// ⚠️ 发送必须在持锁期间完成：锁外发送会与 Unsubscribe 的 close 形成
// send-on-closed 竞态（发送带 default 绝不阻塞，持锁发送开销可忽略）。
func (s *Stream[T]) Publish(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bufCap > 0 {
		s.buf = append(s.buf, v)
		if len(s.buf) > s.bufCap {
			s.buf = s.buf[len(s.buf)-s.bufCap:]
		}
	}
	for ch := range s.subs {
		select {
		case ch <- v:
		default: // 慢订阅者：丢弃本次，不拖慢生产者
		}
	}
}

// Recent 返回最近最多 limit 条历史事件（limit<=0 表示全部）。
func (s *Stream[T]) Recent(limit int) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.buf
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
