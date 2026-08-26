package web

import "net/http"

// ──── 任务启停 ────

func (s *Server) handleTaskStart(w http.ResponseWriter, r *http.Request) {
	if err := s.App.StartTask(r.PathValue("name")); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) handleTaskStop(w http.ResponseWriter, r *http.Request) {
	s.App.StopTask(r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}
