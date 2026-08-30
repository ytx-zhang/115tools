package webui

import (
	"net/http"
	"strconv"
)

// handleSystemLogs 返回系统程序日志（正序）：默认最新 limit 条；?before=<seq> 取该 seq 之前的更旧日志。
func (s *Server) handleSystemLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	before, _ := strconv.ParseUint(r.URL.Query().Get("before"), 10, 64)
	logs, hasMore, err := s.Journal.ListSystemLogs(limit, before)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取系统日志失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "has_more": hasMore})
}

// handleClearSystemLogs 清空全部系统程序日志。
func (s *Server) handleClearSystemLogs(w http.ResponseWriter, r *http.Request) {
	if err := s.Journal.ClearSystemLogs(); err != nil {
		writeErr(w, http.StatusInternalServerError, "清空系统日志失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}
