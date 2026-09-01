package webui

import (
	"net/http"
	"strconv"

	"github.com/ytx-zhang/115tools/internal/logfeed"
)

// handleLogs GET /api/logs?since=N：since 缺省/<=0 返回全量（最新在前），
// 否则返回 seq 之后的增量；响应 {"logs":[...],"seq":N}，seq 为当前最大已分配序号。
// 供前端按需拉取/轮询兜底，SSE 连接正常时前端主要靠实时帧。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	since := 0
	if q := r.URL.Query().Get("since"); q != "" {
		since, _ = strconv.Atoi(q)
	}
	var logs []logfeed.Entry
	if since > 0 {
		logs = s.Logs.Since(uint64(since))
	} else {
		logs = s.Logs.Snapshot()
	}
	if logs == nil {
		logs = []logfeed.Entry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "seq": s.Logs.Seq()})
}

// handleLogsClear DELETE /api/logs：清空内存缓冲（seq 单调递增不复位），返回 204。
func (s *Server) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.Logs.Clear()
	w.WriteHeader(http.StatusNoContent)
}
