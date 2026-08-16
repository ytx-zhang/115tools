// Package logs 提供结构化日志 API：Info/Debug/Warn/Error 输出终端（stdout/stderr 分流）
// 并推送前端 SSE；LogStatus 推送任务进度。底层内嵌泛型 Stream 事件流。
//
// 架构说明：
//   - Stream[T]：泛型 fan-out 事件流（非阻塞广播 + 环形缓冲回放），Hub 与状态推送共用。
//   - levelRouter：把底层 slog handler 按等级分流（≥WARN→stderr，<WARN→stdout）。
//   - logf：四级别函数的统一内部实现（收敛样板，避免四个函数重复 slog.X+logEntry 两行）。
//   - StatusData：任务状态快照的唯一类型，已迁至 internal/status（本包引用其推送状态），web 层与 sync 层共用。
package logs

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/status"
)

// ──── 泛型 Stream ────

// Stream 泛型 fan-out 事件流：Publish 非阻塞广播，环形缓冲供新订阅者回放。
type Stream[T any] struct {
	mu     sync.RWMutex
	buf    []T
	bufCap int
	subs   map[chan T]struct{}
}

// NewHub 创建空 Hub。
func NewHub() *Hub {
	return &Hub{
		stream: &Stream[Entry]{
			bufCap: ringSize,
			subs:   make(map[chan Entry]struct{}),
		},
	}
}

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

// Broadcast 只实时广播不压入环形缓冲（高频瞬态事件如任务状态帧用，
// 避免占满 ring 把历史日志挤出——回放时状态由 handleLogs 手动快照补齐）。
func (s *Stream[T]) Broadcast(v T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- v:
		default:
		}
	}
}

// RecentFiltered 返回最近最多 limit 条满足 match 的历史事件（limit<=0 表示全部）。
// 新在尾部，过滤保留原顺序。
func (s *Stream[T]) RecentFiltered(limit int, match func(T) bool) []T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]T, 0, len(s.buf))
	for _, v := range s.buf {
		if match(v) {
			out = append(out, v)
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

// Reset 清空环形缓冲但保留订阅者（「清空历史」操作，不切断实时推送）。
func (s *Stream[T]) Reset() {
	s.mu.Lock()
	s.buf = s.buf[:0]
	s.mu.Unlock()
}

// ──── 常量 ────

const (
	ringSize = 5000 // 内存中保留的最近日志条数（前端类别查询按此过滤取历史）
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
	ModuleDB     Module = "db"
)

// ──── Entry / Hub ────

// Entry 一条结构化日志快照。Status 仅 LogStatus 调用时填充，普通日志为 nil。
type Entry struct {
	Seq    int64              `json:"seq"` // 全局自增序号：前端可据此检测丢日志/乱序、做增量校验
	Time   time.Time          `json:"time"`
	Level  string             `json:"level"`
	Module string             `json:"module"`
	Msg    string             `json:"msg"`
	Attrs  string             `json:"attrs,omitempty"`
	Status *status.StatusData `json:"status,omitempty"`
}

// moduleList 全部日志模块的有序列表：顺序即 Hub.cntMod 的下标（与前端分类 chip 对齐）。
var moduleList = [...]Module{ModuleSystem, ModuleSync, ModuleStrm, ModuleDrive, ModuleCloud, ModuleDB}

// moduleIndex 返回模块名在 moduleList 中的下标（未知模块归零，语义安全）。
func moduleIndex(name string) int {
	for i, mod := range moduleList {
		if string(mod) == name {
			return i
		}
	}
	return 0
}

// Hub 保存近期日志并向订阅者广播。
type Hub struct {
	stream *Stream[Entry]
	seq    atomic.Int64 // 全局日志序号生成器（见 Write）

	// 权威计数：每次 Write 累加，不受 ring 淘汰影响；前端分类 chip 直接采用，
	// 计数精准（累计自进程启动/上次清空）且无性能代价（O(1) 累加）。
	// 模块固定（moduleList），用固定原子数组替代 map+锁。
	cntTotal atomic.Int64
	cntWarn  atomic.Int64
	cntError atomic.Int64
	cntMod   [len(moduleList)]atomic.Int64
}

// Write 追加一条日志并广播给订阅者。并发安全。
func (h *Hub) Write(e Entry) {
	e.Seq = h.seq.Add(1)
	h.stream.Publish(e)
	// 权威计数：独立于 ring，累加不受淘汰影响；前端分类计数直接采用，准确且 O(1) 无性能代价。
	h.cntTotal.Add(1)
	switch e.Level {
	case "WARN":
		h.cntWarn.Add(1)
	case "ERROR":
		h.cntError.Add(1)
	}
	h.cntMod[moduleIndex(e.Module)].Add(1)
}

// Broadcast 只广播不缓存（任务状态帧用，避免占满 ring 挤出历史日志）。
func (h *Hub) Broadcast(e Entry) { h.stream.Broadcast(e) }

// LogFilter 前端日志分类过滤条件（与前端 chip 一致：all/warn/error/模块名）。
type LogFilter string

const (
	filterAll   LogFilter = "all"
	filterWarn  LogFilter = "warn"
	filterError LogFilter = "error"
)

// Matches 判断日志条目是否属于该分类（用于建立 SSE 连接时按 cat 过滤回放历史）。模块过滤按 Module 精确匹配。
// ⚠️ 与前端 dashboard.js matchFilter 逻辑对称但服务不同切面（前端按 chip 做已渲染行级显隐），不可互相删除。
func (f LogFilter) Matches(e Entry) bool {
	switch f {
	case filterAll:
		return true
	case filterWarn:
		return e.Level == "WARN"
	case filterError:
		return e.Level == "ERROR"
	default:
		return e.Module == string(f)
	}
}

// RecentFiltered 返回最近最多 limit 条满足分类条件的日志（limit<=0 表示全部）。
// 供前端点击类别 chip 时拉取真实对应日志（绕过前端只显示最近 MAX_LINES 的局限）。
func (h *Hub) RecentFiltered(cat LogFilter, limit int) []Entry {
	return h.stream.RecentFiltered(limit, cat.Matches)
}

// Counts 返回各分类权威计数（自进程启动/上次清空累计，不受 ring 淘汰影响）。
// 键与前端 filterKeys 对齐：all/warn/error + 各模块名。前端 chip 直接显示，精准无性能代价。
func (h *Hub) Counts() map[string]int64 {
	m := make(map[string]int64, len(moduleList)+3)
	m["all"] = h.cntTotal.Load()
	m["warn"] = h.cntWarn.Load()
	m["error"] = h.cntError.Load()
	for i, mod := range moduleList {
		m[string(mod)] = h.cntMod[i].Load()
	}
	return m
}

// History 返回某分类中 Seq < before 的最近最多 limit 条日志（升序），供前端向上滚动加载更早历史。
// before<=0 表示不过滤 Seq（取该分类最新 limit 条）。从 ring 直接扫描，O(ring) 但 ring 有界（5000），足够轻量。
func (h *Hub) History(cat LogFilter, before int64, limit int) []Entry {
	// 复用 RecentFiltered 的内部锁与扫描，不直接触碰 stream 私有缓冲；
	// match 内同时施加 Seq<before 游标（before<=0 表示取该分类最新 limit 条）。
	out := h.stream.RecentFiltered(0, func(e Entry) bool {
		return (before <= 0 || e.Seq < before) && cat.Matches(e)
	})
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Subscribe 返回一个接收新日志的通道，需配对调用 Unsubscribe。
func (h *Hub) Subscribe() chan Entry { return h.stream.Subscribe(subBuf) }

// Unsubscribe 移除订阅者并关闭通道。
func (h *Hub) Unsubscribe(ch chan Entry) { h.stream.Unsubscribe(ch) }

// Clear 清空内存中的日志缓冲（不影响实时推送）。
func (h *Hub) Clear() {
	h.stream.Reset()
	h.cntTotal.Store(0)
	h.cntWarn.Store(0)
	h.cntError.Store(0)
	for i := range h.cntMod {
		h.cntMod[i].Store(0)
	}
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

// logEntry 记录一条日志到 Hub 并推送前端；level 由调用方传入 slog.Level（收敛样板）。
// minLevel 过滤在入口 logf 已做，这里直接写入。
// ⚠️ hub 可能为 nil（测试进程未走 main 的 logs.Setup 入口，或启动早期尚未安装 handler）：
// 此时直接丢弃，避免 nil 指针 panic（正常生产路径 Setup 必先于任何日志调用，行为不变）。
func logEntry(module Module, level slog.Level, msg string, args []any) {
	if hub == nil {
		return
	}
	hub.Write(Entry{
		Time:   time.Now(),
		Level:  level.String(),
		Module: string(module),
		Msg:    msg,
		Attrs:  formatAttrs(args),
	})
}

// logf 是四级别函数的统一实现：先打终端 slog（走 levelRouter），再推前端 Hub。
// 低于 minLevel 的日志直接跳过（不落终端、不推前端）。
func logf(level slog.Level, module Module, msg string, args ...any) {
	if level < minLevel {
		return
	}
	switch level {
	case slog.LevelDebug:
		slog.Debug(msg, args...)
	case slog.LevelInfo:
		slog.Info(msg, args...)
	case slog.LevelWarn:
		slog.Warn(msg, args...)
	default:
		slog.Error(msg, args...)
	}
	logEntry(module, level, msg, args)
}

// Info 输出 INFO 级别日志到终端（stdout）并推送前端。
func Info(module Module, msg string, args ...any) { logf(slog.LevelInfo, module, msg, args...) }

// Debug 输出 DEBUG 级别日志到终端（stdout）并推送前端。
func Debug(module Module, msg string, args ...any) { logf(slog.LevelDebug, module, msg, args...) }

// Warn 输出 WARN 级别日志到终端（stderr）并推送前端。
func Warn(module Module, msg string, args ...any) { logf(slog.LevelWarn, module, msg, args...) }

// Error 输出 ERROR 级别日志到终端（stderr）并推送前端。
func Error(module Module, msg string, args ...any) { logf(slog.LevelError, module, msg, args...) }

// LogStatus 推送任务状态快照到前端（不计入终端 slog，不走 levelRouter）。
// ⚠️ 只广播不缓存：状态帧高频产生（每次进度变化），进 ring 会占满缓冲挤出历史日志。
// hub 为 nil 时直接丢弃（同 logEntry 的 nil 守卫，避免未初始化时 panic）。
func LogStatus(status *status.StatusData) {
	if hub == nil {
		return
	}
	hub.Broadcast(Entry{
		Time:   time.Now(),
		Level:  "INFO",
		Module: "status",
		Msg:    "状态快照",
		Status: status,
	})
}
