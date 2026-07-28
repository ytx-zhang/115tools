// Package logstream 在全局 slog 输出之外，额外把每条日志捕获进内存环形缓冲，
// 供管理面板通过 SSE 实时展示「运行日志」。stdout 输出不受影响。
package logstream

import (
	"115tools/event"
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
	Seq   int64     `json:"seq"`
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"`
}

// Hub 保存近期日志并向订阅者广播，底层基于 event.Stream 事件流。
type Hub struct {
	mu     sync.Mutex // 保护 seq 自增（Entry.Seq 赋值）
	seq    int64
	stream *event.Stream[Entry]
}

// NewHub 创建空 Hub。
func NewHub() *Hub {
	return &Hub{stream: event.New[Entry](ringSize)}
}

// Write 追加一条日志并广播给订阅者。并发安全。
func (h *Hub) Write(e Entry) {
	h.mu.Lock()
	h.seq++
	e.Seq = h.seq
	h.mu.Unlock()
	h.stream.Publish(e)
}

// Recent 返回 Seq 大于 after 的日志（最多 limit 条），用于订阅时的历史回放。
func (h *Hub) Recent(after int64, limit int) []Entry {
	return h.stream.Recent(after, limit)
}

// Events 返回日志事件订阅通道（与 syncFile.Runner.Events() 同款手感）。
func (h *Hub) Events() chan Entry { return h.stream.Subscribe(subBuf) }

// Subscribe 返回一个接收新日志的通道，需配对调用 Unsubscribe。
func (h *Hub) Subscribe() chan Entry { return h.stream.Subscribe(subBuf) }

// Unsubscribe 移除订阅者并关闭通道。
func (h *Hub) Unsubscribe(ch chan Entry) { h.stream.Unsubscribe(ch) }

// Clear 清空内存中的日志缓冲（不影响正在进行的实时推送）。
func (h *Hub) Clear() {
	h.mu.Lock()
	h.seq = 0
	h.mu.Unlock()
	h.stream.Reset()
}

// captureHandler 包裹任意 slog.Handler：先交由原 handler 输出（如 stdout），
// 再把记录转发给 Hub，实现「一份日志，两处消费」。
type captureHandler struct {
	slog.Handler
	hub *Hub
}

// WrapHandler 用捕获层包裹 handler，使日志同时进入指定的 logstream Hub。
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
