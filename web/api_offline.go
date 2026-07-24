package web

import (
	"115tools/drive"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const _torrentMaxSize = 10 << 20 // 种子文件最大 10MB

// handleOfflineTasks 分页获取云下载任务列表：GET /api/offline/tasks?page=1
func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	// OfflineTaskList 内部已对 page 做 max(page,1) 守卫，此处无需重复。
	list, err := s.Api.OfflineTaskList(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleOfflineQuota 获取云下载配额。
func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.Api.OfflineQuotaInfo(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取配额失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

// handleOfflineAdd 批量添加离线下载链接。
// save_path 为云端目录路径，留空默认使用 strm_path，"/" 表示网盘根目录。
func (s *Server) handleOfflineAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Urls     string `json:"urls"`
		SavePath string `json:"save_path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}

	var urls []string
	for line := range strings.Lines(req.Urls) {
		if line = strings.TrimSpace(line); line != "" {
			urls = append(urls, line)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少提供一条下载链接")
		return
	}

	dirID, err := s.resolveCloudDir(r, req.SavePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存目录无效: %v", err)
		return
	}

	results, err := s.Api.AddOfflineTasks(r.Context(), urls, dirID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加离线任务失败: %v", err)
		return
	}
	added := 0
	for _, res := range results {
		if res.State {
			added++
		}
	}
	slog.Info("[离线下载] 添加任务", "提交", len(urls), "成功", added, "目录ID", dirID)
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "results": results})
}

// handleOfflineTorrent 上传种子文件并添加 BT 离线任务（multipart/form-data）。
func (s *Server) handleOfflineTorrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(_torrentMaxSize); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传数据失败: %v", err)
		return
	}
	file, hdr, err := r.FormFile("torrent")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "未收到种子文件")
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "读取种子文件失败")
		return
	}

	cfg := s.Cfg.Snapshot()

	// 离线下载保存路径：115 的 save_path 必填非空，取相对根目录的路径串。
	// 前端留空 → 回退 STRM 目录（strm_path）→ 再空回退 "/"（根目录）。
	savePath := strings.TrimSpace(r.FormValue("save_path"))
	if savePath == "" {
		savePath = strings.TrimSpace(cfg.StrmPath)
	}
	savePath = strings.Trim(savePath, "/") // 文档格式为相对路径 "A/B"
	if savePath == "" {
		savePath = "/"
	}

	// 种子文件上传到配置的临时目录（未配置则根目录，"/" 避免回退到 strm_path）
	torrentPath := strings.TrimSpace(cfg.TorrentPath)
	if torrentPath == "" {
		torrentPath = "/"
	}
	torrentCID, err := s.resolveCloudDir(r, torrentPath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "种子临时目录无效: %v", err)
		return
	}
	result, err := s.Api.AddTorrentTask(r.Context(), data, hdr.Filename, torrentCID, savePath)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加种子任务失败: %v", err)
		return
	}

	slog.Info("[离线下载] 添加种子任务",
		"文件名", hdr.Filename,
		"大小", len(data),
		"info_hash", result.InfoHash,
		"保存路径", savePath,
		"成功", result.State,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   boolToInt(result.State),
		"results": []drive.OfflineAddResult{*result},
	})
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// resolveCloudDir 把云端路径解析为目录 ID；空路径回退到 strm_path，"/" 即根目录("0")。
func (s *Server) resolveCloudDir(r *http.Request, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = s.Cfg.Snapshot().StrmPath
	}
	if path == "" || path == "/" {
		return "0", nil
	}
	info, err := s.Api.GetDirInfo(r.Context(), path)
	if err != nil {
		return "", err
	}
	return info.Fid, nil
}

// handleOfflineDelete 删除单个任务，可选同时删除已下载文件。
func (s *Server) handleOfflineDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InfoHash    string `json:"info_hash"`
		DeleteFiles bool   `json:"delete_files"`
	}
	if err := readJSON(r, &req); err != nil || req.InfoHash == "" {
		writeErr(w, http.StatusBadRequest, "缺少 info_hash")
		return
	}
	if err := s.Api.DeleteOfflineTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	slog.Info("[离线下载] 删除任务", "info_hash", req.InfoHash, "删除源文件", req.DeleteFiles)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleOfflineClear 批量清除任务。
// flag：0 已完成，1 全部，2 失败，3 进行中，4 已完成且删源文件，5 全部且删源文件。
func (s *Server) handleOfflineClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Flag int `json:"flag"`
	}
	if err := readJSON(r, &req); err != nil || req.Flag < 0 || req.Flag > 5 {
		writeErr(w, http.StatusBadRequest, "flag 取值范围 0-5")
		return
	}
	if err := s.Api.ClearOfflineTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	slog.Info("[离线下载] 批量清除任务", "flag", req.Flag)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
