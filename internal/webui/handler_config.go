package webui

import (
	"net/http"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/version"
)

// handleVersion 版本探针（公开）。
func handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

// handleGetSettings 返回全局设置快照。
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Conf.Snapshot())
}

// handleSaveSettings 保存全局设置：验证 token → 更新配置 → 热更新缓存 → 重建引擎。
func (s *Server) handleSaveSettings(w http.ResponseWriter, r *http.Request) {
	var req conf.Editable
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}

	// 轻量读旧缓存目录（避免构建整份快照），用于对比是否发生变更
	oldCacheDir := s.Conf.CacheDir()

	// 验证新 refresh_token（非空才验证；空表示不变）
	if req.RefreshToken != "" {
		if _, err := s.Pan.Verify(r.Context(), req.RefreshToken); err != nil {
			writeErr(w, http.StatusUnauthorized, "凭证验证失败: %v", err)
			return
		}
	}

	if err := s.Conf.Update(req); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存配置失败: %v", err)
		return
	}

	// 透传缓存热更新：保留期 + 目录
	if s.Cache != nil {
		cur := s.Conf.Snapshot()
		s.Cache.SetRetention(time.Duration(cur.CacheRetentionDays) * 24 * time.Hour)
		if cur.CacheDir != oldCacheDir {
			if err := s.Cache.SetDir(cur.CacheDir); err != nil {
				journal.Error(r.Context(), "更新缓存目录失败", "错误", err)
			}
		}
	}

	s.SetInitError("")

	// 配置仍未完备（如缺 refresh_token）→ 不启动引擎，仅落盘设置
	if !s.Conf.Status().Ready {
		writeOK(w, http.StatusOK)
		return
	}
	// 幂等启动引擎（首次完备时 Init + Start）
	if err := s.Engine.EnsureRunning(); err != nil {
		s.SetInitError(err.Error())
		writeErr(w, http.StatusInternalServerError, "初始化失败: %v", err)
		return
	}
	// 全局设置变更 → 重建全部任务单元（规则/strm_url/temp_dir 可能变化）
	if err := s.Engine.ReloadAll(); err != nil {
		s.SetInitError(err.Error())
		writeErr(w, http.StatusInternalServerError, "重建任务失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}
