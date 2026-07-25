package web

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// handleStatus SSE 实时推送任务状态（云端同步 / STRM 生成的进度）。
//
// 事件驱动：阻塞等待 syncFile 的状态变更信号，有变更才推送；空闲时零开销。
// 15s 心跳注释帧防止代理/网关空闲超时（504）。
//
// 推送的 JSON 形如 {"ready":true,"config_ready":true,"missing":[],"sync":{...},"strm":{...}}：
//   - ready=false 表示同步器未就绪，需结合 config_ready 区分：
//     · config_ready=false → 配置不完整（前端显示「请补齐配置」横幅 + 缺失项）；
//     · config_ready=true  → 热重载进行中或初始化失败（前端显示「重载中」横幅）。
//   - sync/strm 为各自的任务进度（总数/完成/失败计数/运行中）。
//     失败原因明细不在此推送——统一走 /api/logs 日志管道，
//     前端在日志卡片按「错误」级别过滤即可查看（避免同一错误记两处）。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sw, ok := newSSEWriter(w)
	if !ok {
		slog.Error("[SSE] ResponseWriter 不支持 Flusher，无法建立实时状态流")
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	// 铁律 1：先发注释帧确认连接已建立（触发前端 onopen），再发首帧数据。
	if !sw.writeComment("connected") {
		return
	}
	if !s.sendStatus(sw) {
		return
	}

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	notify := s.StatsNotify

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case <-notify:
			// 状态有变化（进度推进/任务启停/热重载）→ 推送最新快照
			if !s.sendStatus(sw) {
				return
			}
		case <-heartbeat.C:
			// 铁律 2：心跳保活
			if !sw.writeComment("ping") {
				return
			}
		}
	}
}

// sendStatus 写出当前状态快照。返回 false 表示连接已断，调用方应直接 return 关闭流。
func (s *Server) sendStatus(sw *sseWriter) bool {
	ready := s.Syncer() != nil
	configReady := s.Cfg.IsSyncReady()
	missing, _ := json.Marshal(s.Cfg.RequiredMissing())
	var data string
	if ready {
		syncJSON, strmJSON := s.Syncer().StatusSnapshot()
		data = fmt.Sprintf(`{"ready":true,"config_ready":%t,"missing":%s,"sync":%s,"strm":%s}`,
			configReady, missing, syncJSON, strmJSON)
	} else {
		// 未就绪：可能是「配置未填」或「热重载中」，由 config_ready 区分
		data = fmt.Sprintf(`{"ready":false,"config_ready":%t,"missing":%s,"sync":null,"strm":null}`,
			configReady, missing)
	}
	return sw.writeData(data)
}

// handleTaskStart 启动任务：POST /api/task/{name}，name 为 sync（云端同步）或 strm（STRM 生成）。
func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	syncer := s.Syncer()
	if syncer == nil {
		writeErr(w, http.StatusServiceUnavailable, "同步器尚未就绪（可能正在热重载）")
		return
	}
	ctx := s.TaskCtx()
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
	syncer := s.Syncer()
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
