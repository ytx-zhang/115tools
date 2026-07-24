package web

import (
	"encoding/json"
	"net/http"
	"time"

	"115tools/logstream"
)

// logReplayLimit 建立连接时回放的近期日志条数。
const logReplayLimit = 300

// handleLogs SSE 实时推送运行日志（前端「日志」卡片的数据源）。
//
// 流程：先发 ": connected" 注释帧（铁律 1）→ 回放近期日志 → 订阅新日志持续推送；
// 空闲时每 15s 发 ": ping" 心跳（铁律 2）。
//
// 本接口是程序日志的唯一出口：包括错误日志在内都经此推送，
// 前端按级别过滤即可得到「只看错误」的视角（旧的独立错误卡片已合并）。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	sw, ok := newSSEWriter(w)
	if !ok {
		http.Error(w, "不支持流式响应", http.StatusInternalServerError)
		return
	}

	hub := s.Hub

	// 铁律 1：先发注释帧确认连接已建立，触发浏览器 EventSource.onopen
	// （前端在 onopen 中先清空旧内容；本帧必须早于回放，重连时才不会重复）。
	if !sw.writeComment("connected") {
		return
	}

	// 回放近期日志（onopen 已清空 DOM，这里重新填充，不会重复）
	for _, e := range hub.Recent(0, logReplayLimit) {
		if !writeLogEvent(sw, e) {
			return
		}
	}

	sub := hub.Subscribe()
	defer hub.Unsubscribe(sub)

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
		case e := <-sub:
			if !writeLogEvent(sw, e) {
				return
			}
		}
	}
}

// handleLogsClear 清空内存中的运行日志缓冲（前端「清空」按钮）。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.Hub.Clear()
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeLogEvent 写出一条日志事件。返回 false 表示连接已断，
// 调用方应直接 return 优雅关闭流（铁律 3：绝不在失败后再 Flush）。
// 单条日志序列化失败时跳过该条、不影响整条流。
func writeLogEvent(sw *sseWriter, e logstream.Entry) bool {
	data, err := json.Marshal(e)
	if err != nil {
		return true
	}
	return sw.writeData(string(data))
}
