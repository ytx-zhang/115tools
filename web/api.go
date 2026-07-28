package web

import (
	"115tools/config"
	"115tools/logstream"
	"115tools/syncFile"
	"115tools/syncFile/core"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

// ──── SSE 写器（/api/status 与 /api/logs 共用）────
// 可靠性铁律（每条踩过坑，修改时逐条保留）：
//   1. 先发 ": connected" 注释帧再发数据（触发浏览器 onopen）；
//   2. 每 15s ": ping" 心跳防代理 504；
//   3. 写失败立即 return，绝不再 Flush（HTTP/2 会 ERR_HTTP2_PROTOCOL_ERROR）；
//   4. 不设 SetWriteDeadline、不设 Connection 头。

type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	return &sseWriter{w: w, flusher: flusher}, true
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

// ──── 状态 SSE ────

// handleStatus SSE 实时推送任务状态（云端同步/STRM 生成进度）。
// 连接即推当前快照，之后订阅 Sync.Events() 收增量事件——与 fswatcher.Events() 同款手感。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sw, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	events := s.Sync.Events()
	defer s.Sync.Unsubscribe(events)
	if !sw.writeComment("connected") || !s.sendStatus(sw, s.Sync.Snapshot()) {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case ev, ok := <-events:
			if !ok || !s.sendStatus(sw, ev.View) {
				return
			}
		case <-heartbeat.C:
			if !sw.writeComment("ping") {
				return
			}
		}
	}
}

// sendStatus 写出一份状态快照（结构体 json.Marshal，不再手拼 JSON）。
func (s *Server) sendStatus(sw *sseWriter, view *syncFile.StatusView) bool {
	data, _ := json.Marshal(view)
	return sw.writeData(string(data))
}

// ──── 任务启停 ────

// handleTaskStart 启动任务：POST /api/task/{name}，name 为 sync 或 strm。
func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	syncer := s.Sync.Current()
	if syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, "同步器尚未就绪（可能正在热重载）")
		return
	}
	ctx := s.Sync.TaskCtx()
	switch r.PathValue("name") {
	case "sync":
		s.Wg.Go(func() { syncer.StartCloudSync(ctx) })
	case "strm":
		s.Wg.Go(func() { syncer.StartAddStrm(ctx) })
	default:
		writeErr(w, http.StatusNotFound, "未知任务")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleTaskStop 停止任务：DELETE /api/task/{name}。
func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	syncer := s.Sync.Current()
	if syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, "同步器尚未就绪（可能正在热重载）")
		return
	}
	switch r.PathValue("name") {
	case "sync":
		syncer.StopCloudSync()
	case "strm":
		syncer.StopAddStrm()
	default:
		writeErr(w, http.StatusNotFound, "未知任务")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// ──── 配置 ────

// handleGetConfig 返回当前可编辑配置（不含密码明文）。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Cfg.Snapshot())
}

// handleSaveConfig 保存配置并实时生效。三段：
//
//	① refresh_token 校验（有输入才校验落盘，成功后剥离）；
//	② 全局变量刷新（VideoExts/SetUploadExclude 即时生效）；
//	③ 同步器推进（四分支：不完整不启动 / 首次拉起 / 路径类热重载 / 仅排除名单触发全量扫描）。
func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req config.Editable
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}

	// ① refresh_token
	if req.RefreshToken != "" {
		if err := s.Api.VerifyAndApplyRefreshToken(r.Context(), req.RefreshToken); err != nil {
			writeErr(w, http.StatusBadRequest, "refresh_token 校验失败: %v", err)
			return
		}
		req.RefreshToken = ""
	}

	// 更新配置（只覆盖可编辑字段，不丢认证）
	needReload, err := s.Cfg.Update(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	// ② 刷新运行期全局变量
	core.VideoExts = s.Cfg.VideoExts
	core.SetUploadExclude(s.Cfg.UploadExclude)

	// ③ 同步器推进
	missing := s.Cfg.RequiredMissing()
	ready := len(missing) == 0
	started := false
	switch {
	case !ready: // 配置不完整，不启动
	case s.Sync.Current() == nil:
		slog.Info("[WEB] 配置已补齐，启动同步器")
		s.Wg.Go(s.Sync.Reload)
		started = true
	case needReload:
		slog.Info("[WEB] 路径类配置变更，热重载同步器")
		s.Wg.Go(s.Sync.Reload)
		started = true
	}
	// 排除名单变更但未热重载时，触发全量扫描清理云端存量
	if ready && !started && s.Sync.Current() != nil {
		slog.Info("[WEB] 上传排除名单已更新，触发全量扫描清理")
		s.Wg.Go(func() { s.Sync.Current().LocalFullScan(s.Sync.TaskCtx()) })
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"reloading": needReload,
		"ready":     ready,
		"started":   started,
		"missing":   missing,
	})
}

// ──── 日志 SSE ────

const logReplayLimit = 300

// handleLogs SSE 实时推送运行日志。连接时先回放近期日志，再持续推送。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sw, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}
	hub := s.Hub
	if !sw.writeComment("connected") {
		return
	}
	for _, e := range hub.Recent(0, logReplayLimit) {
		if !writeLogEvent(sw, e) {
			return
		}
	}
	events := hub.Events()
	defer hub.Unsubscribe(events)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case <-heartbeat.C:
			if !sw.writeComment("ping") {
				return
			}
		case e, ok := <-events:
			if !ok || !writeLogEvent(sw, e) {
				return
			}
		}
	}
}

// handleLogsClear 清空内存中的运行日志缓冲。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.Hub.Clear()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeLogEvent 写出一条日志事件。序列化失败时跳过（返回 true 不中断流）。
func writeLogEvent(sw *sseWriter, e logstream.Entry) bool {
	data, err := json.Marshal(e)
	if err != nil {
		return true
	}
	return sw.writeData(string(data))
}
