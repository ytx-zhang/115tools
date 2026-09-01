package logfeed

import (
	"context"
	"log/slog"
)

// NewHandler 返回包装 next 的 slog.Handler：
// 级别不低于 collectMin（至少 Warn）的日志在转发 next 写 stdout 的同时写入 feed；
// Enabled 直接转发 next，stdout 输出行为完全不变（本改动零日志侵入）。
func NewHandler(f *Feed, next slog.Handler, collectMin slog.Level) slog.Handler {
	return &handler{feed: f, min: collectMin, next: next}
}

type handler struct {
	feed *Feed
	min  slog.Level // 收集门槛：max(LOG_LEVEL, Warn)，ERROR 环境只收 Error
	next slog.Handler
}

func (h *handler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return h.next.Enabled(ctx, lvl)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= h.min {
		h.feed.Add(Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: formatAttrs(r)})
	}
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{feed: h.feed, min: h.min, next: h.next.WithAttrs(attrs)}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{feed: h.feed, min: h.min, next: h.next.WithGroup(name)}
}

// formatAttrs 提取 Record 的结构化属性为 KV 列表。
func formatAttrs(r slog.Record) []KV {
	var out []KV
	r.Attrs(func(a slog.Attr) bool {
		if a.Equal(slog.Attr{}) {
			return true
		}
		out = append(out, KV{Key: a.Key, Value: a.Value.String()})
		return true
	})
	return out
}
