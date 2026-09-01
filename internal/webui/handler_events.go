package webui

import (
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine"
)

// overview 是推送前端的完整状态快照。
type overview struct {
	ConfigReady bool                 `json:"config_ready"`
	Missing     []string             `json:"missing,omitempty"`
	InitError   string               `json:"init_error,omitempty"`
	Tasks       []engine.TaskRuntime `json:"tasks"`
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

// handleEvents SSE 状态流：连接即回放 overview，之后状态变更实时推送。
//
// 程序日志不在此推送——日志已回归 docker logs；面板里的「最近动态」由 /api/activity 提供按需拉取。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	hubCh, unsubHub := s.Hub.Subscribe()
	defer unsubHub()

	data, err := json.Marshal(s.overviewRef())
	if err != nil {
		return
	}
	if !sw.writeData(string(data)) {
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
			data, err := json.Marshal(s.overviewRef())
			if err != nil {
				continue
			}
			if !sw.writeData(string(data)) {
				return
			}
		}
	}
}
