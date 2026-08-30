package journal

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	store atomic.Pointer[Store]

	sysMu   sync.Mutex
	sysSubs []chan LogEntry
)

type taskKey struct{}

type taskCtx struct {
	taskID string
	runSeq uint64
}

// WithTask 注入任务上下文，使之后产生的日志归入该 run 的明细日志。
func WithTask(ctx context.Context, taskID string, runSeq uint64) context.Context {
	return context.WithValue(ctx, taskKey{}, taskCtx{taskID: taskID, runSeq: runSeq})
}

// Setup 安装全局日志 handler 并绑定执行历史库。须在组合根装配早期调用一次。
// 终端输出级别跟随环境变量 LOG_LEVEL（DEBUG/INFO/WARN/ERROR，缺省/非法回退 INFO）。
func Setup(s *Store) {
	store.Store(s)
	lvl := envLevel()
	h := &handler{
		stdout: slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}),
		stderr: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}),
		min:    lvl,
	}
	slog.SetDefault(slog.New(h))
}

// envLevel 解析 LOG_LEVEL 环境变量；缺省或非法值回退 INFO。
func envLevel() slog.Level {
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Debug / Info / Warn / Error 是日志唯一入口：ctx 携带任务上下文时写入该 run 明细，
// 否则写入系统程序日志（落库 + SSE 广播，全部级别）。
func Debug(ctx context.Context, msg string, kvs ...any) { slog.DebugContext(ctx, msg, kvs...) }
func Info(ctx context.Context, msg string, kvs ...any)  { slog.InfoContext(ctx, msg, kvs...) }
func Warn(ctx context.Context, msg string, kvs ...any)  { slog.WarnContext(ctx, msg, kvs...) }
func Error(ctx context.Context, msg string, kvs ...any) { slog.ErrorContext(ctx, msg, kvs...) }

// handler 是日志路由 handler：min 之下直接丢弃；min 及以上写入 run 明细（有任务 ctx）
// 或横幅（无任务 ctx 且 ≥Warn），并按级别分流 stdout/stderr。
type handler struct {
	stdout slog.Handler
	stderr slog.Handler
	min    slog.Level
}

func (h *handler) Enabled(ctx context.Context, lvl slog.Level) bool { return lvl >= h.min }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	// 明细阈值跟随 LOG_LEVEL：默认 INFO 只记 Info 及以上；DEBUG 级别时调试日志也进任务日志/系统日志。
	if r.Level >= h.min {
		attrs := formatAttrs(r)
		if tc, ok := taskFromCtx(ctx); ok {
			if s := store.Load(); s != nil {
				s.AppendLog(tc.taskID, tc.runSeq, r.Level.String(), r.Message, attrs)
			}
		} else {
			// 无任务上下文：全部级别写入系统程序日志（落库 + SSE 广播）。
			systemLog(r.Level.String(), r.Message, attrs)
		}
	}
	if r.Level >= slog.LevelWarn {
		return h.stderr.Handle(ctx, r)
	}
	return h.stdout.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{stdout: h.stdout.WithAttrs(attrs), stderr: h.stderr.WithAttrs(attrs), min: h.min}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{stdout: h.stdout.WithGroup(name), stderr: h.stderr.WithGroup(name), min: h.min}
}

func taskFromCtx(ctx context.Context) (taskCtx, bool) {
	if ctx == nil {
		return taskCtx{}, false
	}
	tc, ok := ctx.Value(taskKey{}).(taskCtx)
	return tc, ok
}

// formatAttrs 把 slog.Record 的结构化属性拼成 "k=v k2=v2" 字符串，便于落盘与前端展示。
func formatAttrs(r slog.Record) string {
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(a.Value.String())
		return true
	})
	return b.String()
}

// systemLog 写入系统程序日志库并向订阅者广播（慢订阅者丢弃，不阻塞生产者）。
func systemLog(level, msg, attrs string) {
	if s := store.Load(); s != nil {
		entry, err := s.AppendSystemLog(level, msg, attrs)
		if err != nil {
			fmt.Fprintf(os.Stderr, "写入系统日志失败: %v\n", err)
			return
		}
		pushSyslog(entry)
	}
}

// pushSyslog 向全部订阅者非阻塞广播一条系统日志。
func pushSyslog(e LogEntry) {
	sysMu.Lock()
	subs := append([]chan LogEntry(nil), sysSubs...)
	sysMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// SubscribeSystemLog 订阅系统程序日志流；返回通道与取消订阅函数。
func SubscribeSystemLog() (chan LogEntry, func()) {
	ch := make(chan LogEntry, 32)
	sysMu.Lock()
	sysSubs = append(sysSubs, ch)
	sysMu.Unlock()
	return ch, func() {
		sysMu.Lock()
		defer sysMu.Unlock()
		for i, c := range sysSubs {
			if c == ch {
				sysSubs = append(sysSubs[:i], sysSubs[i+1:]...)
				break
			}
		}
	}
}
