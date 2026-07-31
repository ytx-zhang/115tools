package web

import (
	"context"
	"encoding/json"
	"github.com/ytx-zhang/115tools/internal/config"
	synclib "github.com/ytx-zhang/115tools/internal/sync"
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

// serveSSE 是状态/日志两路 SSE 的共用主循环（包级泛型函数：Go 方法不支持类型参数）。
// 连接即发 ": connected" + 首帧回放，之后合流「客户端断开 / AppCtx 取消 / 15s 心跳 /
// 数据事件」四路 select；每个事件统一 json.Marshal 成一帧 data 写出（写失败立即断流，
// 序列化失败跳过本帧不断流）。
func serveSSE[T any](w http.ResponseWriter, r *http.Request, appCtx context.Context, events <-chan T, replay []T) {
	sw, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	if !sw.writeComment("connected") {
		return
	}
	writeFrame := func(v T) bool {
		data, err := json.Marshal(v)
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}
	for _, item := range replay {
		if !writeFrame(item) {
			return
		}
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

// ──── 状态 SSE ────

// handleStatus SSE 实时推送任务状态（云端同步/STRM 生成进度）。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sub := s.Sync.Events()
	defer s.Sync.Unsubscribe(sub)
	serveSSE(w, r, s.AppCtx, sub, []*synclib.StatusView{s.Sync.Snapshot()})
}

// ──── 任务启停 ────

// handleTaskStart 启动任务：POST /api/task/{name}，name 为 sync 或 strm。
func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	if err := s.Sync.StartTask(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// handleTaskStop 停止任务：DELETE /api/task/{name}。
func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	s.Sync.StopTask(r.PathValue("name"))
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

	// 更新配置（只覆盖可编辑字段，不丢认证），返回本次实际变更维度
	cs, oldSyncPath, _, err := s.Cfg.Update(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}
	// ② 刷新运行期全局变量（atomic 更新，避免与扫描协程竞争）
	synclib.SetVideoExts(s.Cfg.VideoExts)
	synclib.SetUploadExclude(s.Cfg.UploadExclude)

	// ③ 同步器推进：据 ChangeSet 精确触发副作用，无兜底分支。
	missing := s.Cfg.RequiredMissing()
	ready := len(missing) == 0
	triggered := false // 本次是否刚拉起/重载了同步器（供前端提示）
	switch {
	case !ready: // 配置不完整，不启动、不重载
	case !s.Sync.Snapshot().Ready:
		// 首次补齐缺失项：拉起同步器
		slog.Info("[WEB] 配置已补齐，启动同步器")
		s.Wg.Go(func() { s.Sync.Reload("") })
		triggered = true
	case cs.PathsChanged || cs.CronChanged:
		// 路径/定时策略变化：热重载同步器（重建实例天然含重扫）。
		slog.Info("[WEB] 路径/定时配置变更，热重载同步器")
		s.Wg.Go(func() {
			s.Sync.Reload(oldSyncPath)
			// 路径重载已重建实例；仅当 strm 直链单独变化（路径未变）才需补一次重写。
			if cs.StrmUrlChanged && !cs.PathsChanged {
				s.Sync.RegenerateStrm(s.Sync.TaskCtx())
			}
		})
		triggered = true
	case cs.StrmUrlChanged:
		// 仅 strm 直链变化：纯本地重写 .strm 内容
		slog.Info("[WEB] strm 直链变更，重写本地 .strm")
		s.Wg.Go(func() { s.Sync.RegenerateStrm(s.Sync.TaskCtx()) })
	case cs.SyncRulesChanged:
		// 排除名单/视频扩展名变化：触发一次全量重扫（清理云端存量 / 重判视频）。
		slog.Info("[WEB] 上传规则已更新，触发全量扫描清理")
		s.Wg.Go(func() { s.Sync.RescanRoot(s.Sync.TaskCtx()) })
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"reloading": triggered,
		"ready":     ready,
		"started":   triggered,
		"missing":   missing,
	})
}

// ──── 日志 SSE ────

const logReplayLimit = 300

// handleLogs SSE 实时推送运行日志。连接时先回放近期日志，再持续推送。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	hub := s.Hub
	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)
	serveSSE(w, r, s.AppCtx, sub, hub.Recent(logReplayLimit))
}

// handleLogsClear 清空内存中的运行日志缓冲。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.Hub.Clear()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
