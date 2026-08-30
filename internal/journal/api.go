package journal

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// bannerCap 横幅环容量（仅保留最近 N 条系统级警告/错误）。
const bannerCap = 20

var (
	store atomic.Pointer[Store]

	bannerMu   sync.Mutex
	banners    []Banner
	bannerSubs []chan Banner
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
// 否则仅终端输出（≥Warn 同时进横幅）。
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
	// run 明细阈值跟随 LOG_LEVEL：默认 INFO 只记 Info 及以上；DEBUG 级别时调试日志也进任务日志。
	if r.Level >= h.min {
		attrs := formatAttrs(r)
		if tc, ok := taskFromCtx(ctx); ok {
			if s := store.Load(); s != nil {
				s.AppendLog(tc.taskID, tc.runSeq, r.Level.String(), r.Message, attrs)
			}
		} else if r.Level >= slog.LevelWarn {
			pushBanner(r.Level.String(), r.Message, attrs)
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

// pushBanner 写入横幅环并广播给订阅者（慢订阅者丢弃，不阻塞）。
func pushBanner(level, msg, attrs string) {
	b := Banner{Level: level, Msg: msg, Attrs: attrs, Time: time.Now()}
	bannerMu.Lock()
	banners = append(banners, b)
	if len(banners) > bannerCap {
		banners = banners[1:]
	}
	broadcast(b)
}

// broadcast 向全部订阅者非阻塞广播一条横幅（慢订阅者丢弃，不阻塞生产者）。
// 调用方必须已持有 bannerMu；本函数负责解锁。
func broadcast(b Banner) {
	subs := append([]chan Banner(nil), bannerSubs...)
	bannerMu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- b:
		default:
		}
	}
}

// Banners 返回当前横幅快照（最新在前，供 SSE 连接时回放）。
func Banners() []Banner {
	bannerMu.Lock()
	defer bannerMu.Unlock()
	return append([]Banner(nil), banners...)
}

// ClearBanners 清空横幅环并广播清空信号（SSE 客户端据此清空本地列表）。
func ClearBanners() {
	bannerMu.Lock()
	banners = banners[:0]
	broadcast(Banner{Cleared: true, Time: time.Now()})
}

// Subscribe 订阅横幅流；返回通道与取消订阅函数。
func Subscribe() (chan Banner, func()) {
	ch := make(chan Banner, 8)
	bannerMu.Lock()
	bannerSubs = append(bannerSubs, ch)
	bannerMu.Unlock()
	return ch, func() {
		bannerMu.Lock()
		defer bannerMu.Unlock()
		for i, c := range bannerSubs {
			if c == ch {
				bannerSubs = append(bannerSubs[:i], bannerSubs[i+1:]...)
				break
			}
		}
	}
}
