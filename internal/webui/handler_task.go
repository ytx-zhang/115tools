package webui

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine"
	"github.com/ytx-zhang/115tools/internal/mirror"
	"github.com/ytx-zhang/115tools/internal/store"
)

// handleListTasks 返回任务列表（配置 + 运行时状态合并）。
func (s *Server) handleListTasks(w http.ResponseWriter, _ *http.Request) {
	tasks := s.Conf.ListTasks()
	runtime := make(map[string]engine.TaskRuntime, len(tasks))
	for _, rt := range s.Engine.Status() {
		runtime[rt.ID] = rt
	}
	type item struct {
		conf.Task
		Running      bool   `json:"running"`
		Initializing bool   `json:"initializing"`
		Queued       bool   `json:"queued"`
		Completed    int64  `json:"completed"`
		Total        int64  `json:"total"`
		Current      string `json:"current,omitempty"`
		LastRun      string `json:"last_run,omitempty"`
		NextCron     string `json:"next_cron,omitempty"`
	}
	out := make([]item, 0, len(tasks))
	for _, t := range tasks {
		rt := runtime[t.ID]
		out = append(out, item{Task: t, Running: rt.Running, Initializing: rt.Initializing,
			Queued: rt.Queued, Completed: rt.Completed, Total: rt.Total,
			Current: rt.Current, LastRun: rt.LastRun, NextCron: rt.NextCron})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": out})
}

// handleCreateTask 新建任务（ID 后端生成）。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	s.saveTask(w, r, conf.NewID(), true, http.StatusCreated)
}

// handleUpdateTask 更新任务。
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	s.saveTask(w, r, r.PathValue("id"), false, http.StatusOK)
}

// saveTask 校验 → 落盘 → 热重建任务运行时（新建与更新共用）。
func (s *Server) saveTask(w http.ResponseWriter, r *http.Request, id string, create bool, okCode int) {
	var t conf.Task
	if err := readJSON(w, r, &t); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	t.ID = id
	// 新建走 AddTask（追加），更新走 UpdateTask（按 ID 覆盖）
	if create {
		if err := s.Conf.AddTask(t); err != nil {
			writeErr(w, http.StatusBadRequest, "保存任务失败: %v", err)
			return
		}
	} else if err := s.Conf.UpdateTask(t); err != nil {
		writeErr(w, http.StatusBadRequest, "保存任务失败: %v", err)
		return
	}
	// 异步重建运行时：首次初始化（构建云端索引）可能耗时数分钟，不能阻塞保存请求。
	// 初始化状态经 engine 的 initializing + SSE 推送实时到达前端，失败只记日志（配置已保存）。
	go func() {
		if err := s.Engine.ReloadTask(t); err != nil {
			slog.ErrorContext(s.AppCtx, "重建任务运行时失败", "任务", t.Name, "任务ID", id, "错误", err)
		}
	}()
	writeJSON(w, okCode, t)
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
	if err := s.Store.DeleteTask(r.Context(), id); err != nil {
		slog.WarnContext(r.Context(), "删除任务活动记录失败", "错误", err)
	}
	// 可选清理该任务本地目录下的索引记录
	if r.URL.Query().Get("purge") == "1" {
		s.Store.ClearTree(r.Context(), task.LocalDir)
	}
	writeOK(w, http.StatusOK)
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

// handleDryRun 预演：只算计划不执行，返回将要发生的动作清单。
// scope=download 显式看云端→本地；未指定时按任务方向推断：纯下载任务默认下载方向，
// 其余（双向/纯上传）默认上传方向，避免纯下载任务预演误显示上传动作。
func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	scope := store.ScopeUpload
	if sc := r.URL.Query().Get("scope"); sc == "download" {
		scope = store.ScopeDownload
	} else if sc == "" {
		if task, ok := s.Conf.GetTask(r.PathValue("id")); ok && !task.UploadEnabled() {
			scope = store.ScopeDownload
		}
	}
	ops, err := s.Engine.DryRun(r.PathValue("id"), scope)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "预览失败: %v", err)
		return
	}

	// 按动作类型分组计数 + 汇总危险动作，前端据此渲染分组与红色警示
	groups := make([]map[string]any, 0, 8)
	var danger int64
	seen := map[mirror.OpKind]bool{}
	byKind := map[mirror.OpKind]int64{}
	for _, op := range ops {
		byKind[op.Kind]++
		if op.Kind.Danger() {
			danger++
		}
	}
	for _, op := range ops {
		if seen[op.Kind] {
			continue
		}
		seen[op.Kind] = true
		groups = append(groups, map[string]any{
			"op":     int(op.Kind),
			"label":  op.Kind.Label(),
			"count":  byKind[op.Kind],
			"danger": op.Kind.Danger(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ops":    ops,
		"groups": groups,
		"danger": danger,
	})
}

// handleActivity 返回活动事件（?task_id= 过滤，?offset= 起始条数，?limit= 条数）。
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	events := s.Store.List(r.Context(), r.URL.Query().Get("task_id"), offset, limit)
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleActivityClear 清空活动事件（?task_id= 限定单任务，缺省为全部）。
func (s *Server) handleActivityClear(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 task_id 参数")
		return
	}
	if err := s.Store.DeleteTask(r.Context(), taskID); err != nil {
		writeErr(w, http.StatusInternalServerError, "清空活动记录失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}
