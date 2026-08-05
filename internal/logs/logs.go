// Package logs 提供结构化日志 API：Info/Debug/Warn/Error 输出终端（stdout/stderr 分流）
// 并推送前端 SSE；LogStatus 推送任务进度。底层内嵌泛型 Stream 事件流。
package logs

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// ──── 泛型 Stream ────

// Stream 泛型 fan-out 事件流：Publish 非阻塞广播，环形缓冲供新订阅者回放。
type Stream[T any] struct {
	mu     sync.RWMutex
	buf    []T
	bufCap int
	subs   map[chan T]struct{}
}

// NewStream 创建事件流，ring 为环形缓冲容量（0 表示不保留历史）。
func NewStream[T any](ring int) *Stream[T] {
	return &Stream[T]{
		bufCap: ring,
		subs:   make(map[chan T]struct{}),
	}
}

// Publish 非阻塞广播一次事件，同时压入环形缓冲。
// 慢订阅者丢弃本次推送，不拖慢生产者。
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
		default:
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

// Reset 清空环形缓冲但保留订阅者（「清空历史」操作，不切断实时推送）。
func (s *Stream[T]) Reset() {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.mu.Unlock()
}

// ──── 常量 ────

const (
	ringSize = 1000 // 内存中保留的最近日志条数
	subBuf   = 128  // SSE 订阅者发送缓冲
)

// ──── Module ────

// Module 日志来源模块类型。
type Module string

const (
	ModuleSystem Module = "system"
	ModuleSync   Module = "sync"
	ModuleStrm   Module = "strm"
	ModuleDrive  Module = "drive"
	ModuleCloud  Module = "cloud"
	ModuleWeb    Module = "web"
	ModuleDB     Module = "db"
)

// ──── Entry / Hub ────

// Entry 一条结构化日志快照。Status 仅 LogStatus 调用时填充，普通日志为 nil。
type Entry struct {
	Time   time.Time   `json:"time"`
	Level  string      `json:"level"`
	Module string      `json:"module"`
	Msg    string      `json:"msg"`
	Attrs  string      `json:"attrs,omitempty"`
	Status *StatusData `json:"status,omitempty"`
}

// Hub 保存近期日志并向订阅者广播。
type Hub struct {
	stream *Stream[Entry]
}

// NewHub 创建空 Hub。
func NewHub() *Hub {
	return &Hub{stream: NewStream[Entry](ringSize)}
}

// Write 追加一条日志并广播给订阅者。并发安全。
func (h *Hub) Write(e Entry) { h.stream.Publish(e) }

// Recent 返回最近最多 limit 条日志（limit<=0 表示全部）。
func (h *Hub) Recent(limit int) []Entry { return h.stream.Recent(limit) }

// Subscribe 返回一个接收新日志的通道，需配对调用 Unsubscribe。
func (h *Hub) Subscribe() chan Entry { return h.stream.Subscribe(subBuf) }

// Unsubscribe 移除订阅者并关闭通道。
func (h *Hub) Unsubscribe(ch chan Entry) { h.stream.Unsubscribe(ch) }

// Clear 清空内存中的日志缓冲（不影响实时推送）。
func (h *Hub) Clear() { h.stream.Reset() }

// ──── TaskStatus / StatusData ────

// TaskStatus 单任务进度快照（供 web 层 SSE 消费）。
type TaskStatus struct {
	Running   bool  `json:"running"`
	Completed int64 `json:"completed"`
	Total     int64 `json:"total"`
}

// StatusData 推送前端的完整任务状态快照。
type StatusData struct {
	Ready       bool        `json:"ready"`
	ConfigReady bool        `json:"config_ready"`
	Missing     []string    `json:"missing,omitempty"`
	Sync        *TaskStatus `json:"sync"`
	Strm        *TaskStatus `json:"strm"`
}

// ──── levelRouter ────

// levelRouter 把底层 slog handler 按等级分流：≥WARN→stderr，<WARN→stdout。
type levelRouter struct {
	stdout slog.Handler
	stderr slog.Handler
}

func (r *levelRouter) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= slog.LevelWarn {
		return r.stderr.Enabled(ctx, level)
	}
	return r.stdout.Enabled(ctx, level)
}

func (r *levelRouter) Handle(ctx context.Context, rec slog.Record) error {
	if rec.Level >= slog.LevelWarn {
		return r.stderr.Handle(ctx, rec)
	}
	return r.stdout.Handle(ctx, rec)
}

func (r *levelRouter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelRouter{r.stdout.WithAttrs(attrs), r.stderr.WithAttrs(attrs)}
}

func (r *levelRouter) WithGroup(name string) slog.Handler {
	return &levelRouter{r.stdout.WithGroup(name), r.stderr.WithGroup(name)}
}

// ──── Setup ────

var hub *Hub
var minLevel slog.Level

// Setup 配置全局 slog：解析 LOG_LEVEL 环境变量，用 levelRouter 分流 stdout/stderr，
// 存储 hub 为包级变量。调用方：main 启动时调用一次。
func Setup(h *Hub) {
	hub = h
	minLevel = parseLevel()
	level := minLevel

	opts := &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.String(slog.TimeKey, a.Value.Time().Format("15:04:05.000"))
			}
			return a
		},
	}

	router := &levelRouter{
		stdout: slog.NewTextHandler(os.Stdout, opts),
		stderr: slog.NewTextHandler(os.Stderr, opts),
	}
	slog.SetDefault(slog.New(router))
}

func parseLevel() slog.Level {
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// ──── 公开 API ────

func formatAttrs(args []any) string {
	if len(args) == 0 {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < len(args); i += 2 {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(toString(args[i]))
		sb.WriteByte('=')
		if i+1 < len(args) {
			sb.WriteString(toString(args[i+1]))
		} else {
			sb.WriteString("<MISSING>")
		}
	}
	return sb.String()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return slog.AnyValue(v).String()
}

func logEntry(module Module, level string, msg string, args []any) {
	var lv slog.Level
	switch level {
	case "DEBUG":
		lv = slog.LevelDebug
	case "INFO":
		lv = slog.LevelInfo
	case "WARN":
		lv = slog.LevelWarn
	case "ERROR":
		lv = slog.LevelError
	}
	if lv < minLevel {
		return
	}
	attrsStr := formatAttrs(args)
	hub.Write(Entry{
		Time:   time.Now(),
		Level:  level,
		Module: string(module),
		Msg:    msg,
		Attrs:  attrsStr,
	})
}

// Info 输出 INFO 级别日志到终端（stdout）并推送前端。
func Info(module Module, msg string, args ...any) {
	slog.Info(msg, args...)
	logEntry(module, "INFO", msg, args)
}

// Debug 输出 DEBUG 级别日志到终端（stdout）并推送前端。
func Debug(module Module, msg string, args ...any) {
	slog.Debug(msg, args...)
	logEntry(module, "DEBUG", msg, args)
}

// Warn 输出 WARN 级别日志到终端（stderr）并推送前端。
func Warn(module Module, msg string, args ...any) {
	slog.Warn(msg, args...)
	logEntry(module, "WARN", msg, args)
}

// Error 输出 ERROR 级别日志到终端（stderr）并推送前端。
func Error(module Module, msg string, args ...any) {
	slog.Error(msg, args...)
	logEntry(module, "ERROR", msg, args)
}

// LogStatus 推送任务状态快照到前端（不计入终端 slog，不走 levelRouter）。
func LogStatus(status *StatusData) {
	hub.Write(Entry{
		Time:   time.Now(),
		Level:  "INFO",
		Module: "status",
		Msg:    "状态快照",
		Status: status,
	})
}
