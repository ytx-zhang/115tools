package web

import (
	"115tools/config"
	"log/slog"
	"net/http"
)

// handleGetConfig 返回当前可编辑配置（不含密码明文）。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Cfg.Snapshot())
}

// handleSaveConfig 保存配置并实时生效：
//   - 登录凭据变更立即生效（中间件每次请求都读取最新配置）；
//   - 路径 / STRM URL / 静默窗口变更触发同步器热重载（异步，不阻塞响应）。
//
// 注意：请求不携带 token 字段，Update 内部只覆盖可编辑字段，认证与 token 不丢失。
func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req config.Editable
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}

	// refresh_token 单独处理：仅在用户有输入时才校验并落盘，避免无效 token 写盘后
	// 旧 access_token 过期导致刷新全挂。校验成功即已持久化（含 115 可能轮换的新 rt），
	// 从请求剥离后交给 Update 处理其余字段。
	if req.RefreshToken != "" {
		if err := s.Api.VerifyAndApplyRefreshToken(r.Context(), req.RefreshToken); err != nil {
			writeErr(w, http.StatusBadRequest, "refresh_token 校验失败，未保存: %v", err)
			return
		}
		req.RefreshToken = ""
	}

	needReload, err := s.Cfg.Update(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	if needReload {
		slog.Info("[WEB] 路径类配置变更，开始热重载同步器")
		s.Wg.Go(s.Reload)
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "reloading": needReload})
}
