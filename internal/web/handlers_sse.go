package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── SSE 写器 ────

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (s *sseWriter) writeData(payload string) bool {
	if _, err := s.w.Write([]byte("data: " + payload + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

func (s *sseWriter) writeComment(msg string) bool {
	if _, err := s.w.Write([]byte(":" + msg + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

// sseConnect 完成 SSE 响应头设置并发送首条 connected 注释帧；不支持流式或写入首帧失败时
// 返回 (nil, false)，由调用方写错误响应。serveSSE 与 handleLogsCounts 共用，避免重复样板。
func sseConnect(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sw := &sseWriter{w: w, flusher: flusher}
	if !sw.writeComment("connected") {
		return nil, false
	}
	return sw, true
}

// serveSSE 把 events 流实时推给单个订阅者，并在连接建立时先回放 replay。
// match 可选：非 nil 时回放与实时事件都先经它过滤（分类日志 SSE 用）；nil 表示不过滤。
func serveSSE(w http.ResponseWriter, r *http.Request, appCtx context.Context, events <-chan logs.Entry, replay []logs.Entry, match func(logs.Entry) bool) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	pass := func(v logs.Entry) bool { return match == nil || match(v) }
	// 回放打包为单个数据帧（JSON 数组），避免 1000 条逐条写+Flush 拖慢首屏。
	if len(replay) > 0 {
		if match != nil {
			// 用 slices.DeleteFunc 在克隆上过滤，避免改动调用方持有的 replay 底层数组
			//（RecentFiltered 返回的切片可能被上层复用，原地写入会污染回放缓冲）。
			replay = slices.DeleteFunc(slices.Clone(replay), func(v logs.Entry) bool {
				return !pass(v)
			})
		}
		if len(replay) > 0 {
			data, err := json.Marshal(replay)
			if err == nil && !sw.writeData(string(data)) {
				return
			}
		}
	}
	writeFrame := func(v logs.Entry) bool {
		if !pass(v) {
			return true
		}
		data, err := json.Marshal(v)
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-appCtx.Done():
			return
		case <-heartbeat.C:
			if !sw.writeComment("ping") {
				return
			}
		case ev, ok := <-events:
			if !ok || !writeFrame(ev) {
				return
			}
		}
	}
}

// ──── 日志 SSE ────

// logReplayLimit 切换分类时 SSE 回放的最近条数：只够首屏一屏，更早的由 /api/logs/history 滚动翻页按需取。
const logReplayLimit = 300

// handleLogsCounts 分类日志计数流：事件驱动，仅在有新日志写入（计数可能变化）时推送，
// 空闲不推送——替代原 300ms 定时轮询（无日志时仍高频空推的浪费）。计数直接扫描 ring
// （与回放/翻页同一数据源），保证「chip 显示有日志 ⇔ 点进去能看到日志」严格一致；
// 早期日志被 ring 淘汰后计数同步回落，不会出现计数有、内容无的矛盾。
// 与 handleLogs（按 cat 过滤的日志流）分离：计数全局、日志流按分类过滤，故需独立流；此处订阅日志流，
// 有日志即推计数，与「日志推送即计数更新」语义一致。
func (s *Server) handleLogsCounts(w http.ResponseWriter, r *http.Request) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	sub := s.App.Subscribe()
	defer s.App.Unsubscribe(sub)

	send := func() bool {
		data, err := json.Marshal(map[string]any{"counts": s.App.LogCounts()})
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}
	if !send() { // 连接即推送当前计数，保证 chip 立即可见
		return
	}

	// 事件驱动：订阅日志流，任意日志写入即标记脏，150ms 内合并推送一次（突发日志不洪泛）；
	// 空闲时不推送。与 serveSSE 对齐保留 15s 心跳，避免空闲连接被反向代理掐断。
	// 每次推送重新扫描 ring 取最新可见计数，即使订阅丢帧也不影响准确性（只把 entry 当脏信号）。
	debounce := time.NewTimer(time.Hour)
	debounce.Stop()
	select {
	case <-debounce.C:
	default:
	}
	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	var dirty bool
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case <-hb.C:
			if !sw.writeComment("ping") {
				return
			}
		case _, ok := <-sub:
			if !ok {
				return
			}
			if !dirty {
				dirty = true
				debounce.Reset(150 * time.Millisecond)
			}
		case <-debounce.C:
			if !send() {
				return
			}
			dirty = false
		}
	}
}

// handleLogs 单一日志通道：支持 ?cat= 分类参数。后端按分类过滤回放历史并实时推送，
// 前端切换分类时断开重建本连接即可，无需再走独立的历史查询接口。
// cat=all|warn|error|模块名；缺省 all。状态帧（Module="status"）始终推送，不参与分类过滤。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sub := s.App.Subscribe()
	defer s.App.Unsubscribe(sub)

	cat := r.URL.Query().Get("cat")
	if cat == "" {
		cat = "all"
	}
	filter := logs.LogFilter(cat)
	// 状态帧始终放行；其余按分类过滤（回放与实时共用）。
	match := func(e logs.Entry) bool {
		if e.Status != nil {
			return true
		}
		return filter.Matches(e)
	}

	// 状态快照作为首条事件回放（直接复用 App.Snapshot 单一类型）
	snap := s.App.Snapshot()
	// 回放该分类最近 logReplayLimit 条（而非 ring 全部）：首屏只需一屏，更早的由
	// /api/logs/history 滚动翻页按需取（ring 仍有全量）。避免一次性推送 5000 条、前端只渲染 300 条的浪费。
	replay := s.App.RecentFiltered(cat, logReplayLimit)
	replay = slices.Concat([]logs.Entry{{
		Time: time.Now(), Level: "INFO", Module: "status", Msg: "状态快照",
		Status: snap,
	}}, replay)

	serveSSE(w, r, s.AppCtx, sub, replay, match)
}

func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.App.ClearLogs()
	writeOK(w, http.StatusOK)
}

// handleLogsHistory 向前翻页：返回某分类中 Seq<before 的最近最多 limit 条日志（升序），
// 供前端向上滚动加载更早历史。before 为当前视图顶部日志的 seq（缺失/0 表示取最新 limit 条）。
func (s *Server) handleLogsHistory(w http.ResponseWriter, r *http.Request) {
	cat := r.URL.Query().Get("cat")
	if cat == "" {
		cat = "all"
	}
	before, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64)
	if err != nil {
		before = 0 // 缺失/非法 → 取最新 limit 条
	}
	limit, err := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if err != nil {
		limit = 0 // 缺失/非法 → 走下方默认 200
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	writeJSON(w, http.StatusOK, s.App.LogHistory(cat, before, limit))
}
