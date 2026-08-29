package webui

import (
	"net/http"
	"strconv"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// handleListTasks 返回任务列表（配置 + 运行时状态合并）。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	tasks := s.Conf.ListTasks()
	runtime := make(map[string]engine.TaskRuntime, len(tasks))
	for _, rt := range s.Engine.Status() {
		runtime[rt.ID] = rt
	}
	type item struct {
		conf.Task
		Running   bool  `json:"running"`
		Completed int64 `json:"completed"`
		Total     int64 `json:"total"`
	}
	out := make([]item, 0, len(tasks))
	for _, t := range tasks {
		rt := runtime[t.ID]
		out = append(out, item{Task: t, Running: rt.Running, Completed: rt.Completed, Total: rt.Total})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// handleCreateTask 新建任务（ID 后端生成）。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var t conf.Task
	if err := readJSON(w, r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	t.ID = conf.NewID()
	if err := s.Conf.AddTask(t); err != nil {
		writeErr(w, http.StatusBadRequest, "保存任务失败: %v", err)
		return
	}
	if err := s.Engine.ReloadTask(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "启动任务失败: %v", err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// handleUpdateTask 更新任务。
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var t conf.Task
	if err := readJSON(w, r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	t.ID = id
	if err := s.Conf.UpdateTask(t); err != nil {
		writeErr(w, http.StatusBadRequest, "保存任务失败: %v", err)
		return
	}
	if err := s.Engine.ReloadTask(t); err != nil {
		writeErr(w, http.StatusInternalServerError, "重建任务失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleDeleteTask 删除任务（?purge=1 同时清理该任务的本地路径索引）。
func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := s.Conf.GetTask(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "任务不存在: %s", id)
		return
	}
	if err := s.Conf.RemoveTask(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "删除任务失败: %v", err)
		return
	}
	s.Engine.RemoveTask(id)
	if err := s.Journal.DeleteTask(id); err != nil {
		journal.Warn(r.Context(), "删除任务历史失败", "错误", err)
	}
	// 可选清理该任务本地目录下的索引记录
	if r.URL.Query().Get("purge") == "1" {
		s.purgeTaskIndex(r, task)
	}
	writeOK(w, http.StatusOK)
}

// purgeTaskIndex 清理某任务本地目录下的全部索引记录。
func (s *Server) purgeTaskIndex(r *http.Request, task conf.Task) {
	if s.Vault == nil {
		return
	}
	s.Vault.ClearPaths(r.Context(), []string{task.LocalDir})
}

// handleStartTask 手动执行任务。
func (s *Server) handleStartTask(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.StartTask(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "%v", err)
		return
	}
	writeOK(w, http.StatusAccepted)
}

// handleStopTask 停止任务。
func (s *Server) handleStopTask(w http.ResponseWriter, r *http.Request) {
	s.Engine.StopTask(r.PathValue("id"))
	writeOK(w, http.StatusAccepted)
}

// handleClearTaskRuns 清空某任务的执行历史与明细日志。
func (s *Server) handleClearTaskRuns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Journal.DeleteTask(id); err != nil {
		writeErr(w, http.StatusInternalServerError, "清空执行历史失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}

// handleTaskRuns 返回某任务的执行历史（最新在前）。
func (s *Server) handleTaskRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := s.Journal.List(r.PathValue("id"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取执行历史失败: %v", err)
		return
	}
	if runs == nil {
		runs = []journal.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleTaskRunLogs 返回某次执行的明细日志。
func (s *Server) handleTaskRunLogs(w http.ResponseWriter, r *http.Request) {
	seq, err := strconv.ParseUint(r.PathValue("seq"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "seq 非法")
		return
	}
	logs, err := s.Journal.Logs(seq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取执行日志失败: %v", err)
		return
	}
	if logs == nil {
		logs = []journal.LogEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}
