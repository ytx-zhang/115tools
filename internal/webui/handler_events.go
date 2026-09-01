package webui

import (
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/logfeed"
)

// overview 是推送前端的完整状态快照。
type overview struct {
	ConfigReady bool                 `json:"config_ready"`
	Missing     []string             `json:"missing,omitempty"`
	InitError   string               `json:"init_error,omitempty"`
	Tasks       []engine.TaskRuntime `json:"tasks"`
}

// overviewMsg SSE overview 帧（type 字段区分消息类型）。
type overviewMsg struct {
	Type string `json:"type"` // "overview"
	overview
}

// logsMsg SSE 日志帧：连接回放全量（full=true），之后推增量（full=false）。
// seq 为当前最大已分配序号，前端据此记录已见位置，断线重连后以全量帧重置。
type logsMsg struct {
	Type string          `json:"type"` // "logs"
	Full bool            `json:"full"`
	Seq  uint64          `json:"seq"`
	Logs []logfeed.Entry `json:"logs"`
}

// handleOverview 返回当前状态快照（非 SSE，供初次加载兜底）。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.overviewRef())
}

// overviewRef 返回状态快照指针（复用于 HTTP 接口与 SSE 帧）。
func (s *Server) overviewRef() *overview {
	st := s.Conf.Status()
	return &overview{
		ConfigReady: st.Ready,
		Missing:     st.Missing,
		InitError:   s.getInitError(),
		Tasks:       s.Engine.Status(),
	}
}

// sseWriter SSE 写器。
type sseWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func sseConnect(w http.ResponseWriter) (*sseWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	sw := &sseWriter{w: w, flusher: flusher}
	if !sw.comment("connected") {
		return nil, false
	}
	return sw, true
}

// writeChunk 写出一个 SSE 块（data 或注释）并立即 flush；写失败返回 false。
func (s *sseWriter) writeChunk(prefix, payload string) bool {
	if _, err := s.w.Write([]byte(prefix + payload + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

func (s *sseWriter) writeData(payload string) bool { return s.writeChunk("data: ", payload) }

func (s *sseWriter) comment(msg string) bool { return s.writeChunk(":", msg) }

// writeSSE 序列化并写出一个 SSE 帧；序列化或写失败返回 false（调用方据此断开连接）。
func writeSSE(sw *sseWriter, v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return false
	}
	return sw.writeData(string(data))
}

// handleEvents SSE 状态流：连接即回放 overview 与日志全量，之后变更实时推送。
//
// 帧协议（data 内 type 字段区分，不加命名事件）：
//   - {"type":"overview", ...}          状态快照（连接回放 / 引擎状态变更）
//   - {"type":"logs","full":true, ...}  日志全量（连接回放，断线重连后整体替换）
//   - {"type":"logs","full":false, ...} 日志增量（新 Warn/Error 实时推送）
//
// 程序日志不在此推送——日志已回归 docker logs；Warn/Error 级别经 logfeed 收集后推送，
// 供前端任务中心右下角提醒按钮与日志弹窗消费。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	hubCh, unsubHub := s.Hub.Subscribe()
	defer unsubHub()
	feedCh, unsubFeed := s.Logs.Subscribe()
	defer unsubFeed()

	// 连接回放：overview + 日志全量（lastLogSeq = 全量内最大 seq，连接窗口内的新日志由增量帧补齐）
	if !writeSSE(sw, overviewMsg{Type: "overview", overview: *s.overviewRef()}) {
		return
	}
	snap := s.Logs.Snapshot()
	var lastLogSeq uint64
	if len(snap) > 0 {
		lastLogSeq = snap[0].Seq // 最新在前，首条 seq 最大
	}
	if !writeSSE(sw, logsMsg{Type: "logs", Full: true, Seq: lastLogSeq, Logs: snap}) {
		return
	}

	hb := time.NewTicker(15 * time.Second)
	defer hb.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.AppCtx.Done():
			return
		case <-hb.C:
			if !sw.comment("ping") {
				return
			}
		case <-hubCh:
			if !writeSSE(sw, overviewMsg{Type: "overview", overview: *s.overviewRef()}) {
				return
			}
		case <-feedCh:
			seq := s.Logs.Seq()
			if !writeSSE(sw, logsMsg{Type: "logs", Full: false, Seq: seq, Logs: s.Logs.Since(lastLogSeq)}) {
				return
			}
			lastLogSeq = seq
		}
	}
}
