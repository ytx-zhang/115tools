package webui

import (
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// overview 是推送前端的完整状态快照。
type overview struct {
	ConfigReady bool                 `json:"config_ready"`
	Missing     []string             `json:"missing,omitempty"`
	InitError   string               `json:"init_error,omitempty"`
	Tasks       []engine.TaskRuntime `json:"tasks"`
}

// event 是 SSE 推送的帧（type 区分 overview / banner）。
type event struct {
	Type     string          `json:"type"`
	Overview *overview       `json:"overview,omitempty"`
	Banner   *journal.Banner `json:"banner,omitempty"`
}

// handleOverview 返回当前状态快照（非 SSE，供初次加载兜底）。
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.overview())
}

// handleClearBanners 清空系统级错误/警告横幅（广播清空信号，SSE 客户端同步清空本地列表）。
func (s *Server) handleClearBanners(w http.ResponseWriter, r *http.Request) {
	journal.ClearBanners()
	writeOK(w, http.StatusOK)
}

func (s *Server) overview() overview {
	st := s.Conf.Status()
	return overview{
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

func (s *sseWriter) writeData(payload string) bool {
	if _, err := s.w.Write([]byte("data: " + payload + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

func (s *sseWriter) comment(msg string) bool {
	if _, err := s.w.Write([]byte(":" + msg + "\n\n")); err != nil {
		return false
	}
	s.flusher.Flush()
	return true
}

// handleEvents SSE 状态流：连接即回放 overview + 最近横幅，之后状态变更/新横幅实时推送。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	sw, ok := sseConnect(w)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	hubCh, unsubHub := s.Hub.Subscribe()
	defer unsubHub()
	bannerCh, unsubBanner := journal.Subscribe()
	defer unsubBanner()

	send := func(e event) bool {
		data, err := json.Marshal(e)
		if err != nil {
			return true
		}
		return sw.writeData(string(data))
	}

	// 回放：overview + 最近横幅
	if !send(event{Type: "overview", Overview: s.overviewRef()}) {
		return
	}
	for _, b := range journal.Banners() {
		b := b
		if !send(event{Type: "banner", Banner: &b}) {
			return
		}
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
			if !send(event{Type: "overview", Overview: s.overviewRef()}) {
				return
			}
		case b := <-bannerCh:
			if !send(event{Type: "banner", Banner: &b}) {
				return
			}
		}
	}
}

// overviewRef 返回 overview 的指针（复用于回放与实时推送）。
func (s *Server) overviewRef() *overview {
	o := s.overview()
	return &o
}
