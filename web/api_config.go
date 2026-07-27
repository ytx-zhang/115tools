package web

import (
	"115tools/config"
	"115tools/syncFile/core"
	"log/slog"
	"net/http"
)

// handleGetConfig 返回当前可编辑配置（不含密码明文）。
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Cfg.Snapshot())
}

// handleSaveConfig 保存配置并实时生效：
//   - 登录凭据变更立即生效（中间件每次请求都读取最新配置）；
//   - 路径 / STRM URL / 静默窗口变更触发同步器热重载（异步，不阻塞响应）；
//   - 配置不完整时仍允许保存（便于分步填写），但不会启动同步器，
//     待用户在面板补齐、保存后由本函数自动拉起同步器。
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

	// 视频扩展名白名单改动即时生效：运行期分类直接读 core.VideoExts，
	// 这里在 Update 落盘后刷新（即使未触发同步器热重载也要生效）。
	if len(s.Cfg.VideoExts) > 0 {
		core.VideoExts.Store(&s.Cfg.VideoExts)
	}

	// 上传排除名单改动即时生效：readLocalDir 实时读 core.IsUploadExcluded，
	// 这里在 Update 落盘后刷新；之后触发一次全量扫描，把云端已误传的临时文件
	// 一并联动清理（不必等重启或下一个定时全量周期）。
	core.SetUploadExclude(s.Cfg.UploadExclude)

	// Emby 排除文件随 video_exts / 开关 / SyncPath 变化刷新（不进 needReload）。
	s.Cfg.SyncEmbyIgnore()

	// 依据配置完整性决定如何推进同步器：
	//   - 仍不完整：不启动，前端据此提示补齐（started=false）；
	//   - 已补齐且同步器从未运行：首次拉起同步器（Reload 在 cancel 为 nil 时直接 startLocked）；
	//   - 已就绪且路径类变更：热重载同步器。
	missing := s.Cfg.RequiredMissing() // 算一次，ready 与 missing 都从它派生
	ready := len(missing) == 0
	started := false
	switch {
	case !ready:
		// 不启动：配置不完整，前端提示用户补齐缺失项。
	case s.Syncer() == nil:
		slog.Info("[WEB] 配置已补齐，启动同步器")
		s.Wg.Go(s.Reload)
		started = true
	case needReload:
		slog.Info("[WEB] 路径类配置变更，开始热重载同步器")
		s.Wg.Go(s.Reload)
		started = true
	}
	// 上传排除名单改动后，若同步器原本已在运行（非首次启动/非热重载路径），
	// 立即触发一次全量扫描，把云端已误传的临时文件联动清理；
	// 首次启动/热重载路径下 local.Start 已自带全量扫描，无需重复触发。
	if ready && !started && s.Syncer() != nil {
		slog.Info("[WEB] 上传排除名单已更新，触发全量扫描清理云端存量临时文件")
		s.Wg.Go(func() { s.Syncer().LocalFullScan(s.TaskCtx()) })
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"reloading": needReload,
		"ready":     ready,
		"started":   started,
		"missing":   missing,
	})
}
