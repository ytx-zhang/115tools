package web

import (
	"net/http"

	"github.com/ytx-zhang/115tools/internal/config"
)

// ──── 配置 ────

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.App.ConfigSnapshot())
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req config.Editable
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if err := s.App.ApplyConfig(r.Context(), req); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
