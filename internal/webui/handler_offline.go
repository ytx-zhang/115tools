package webui

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
)

const torrentMaxSize = 10 << 20

// handleOfflineTasks 离线任务列表（page 从 1 开始）。
func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	list, err := s.Pan.ListTasks(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleOfflineQuota 离线下载配额。
func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.Pan.GetQuota(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取配额失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

// handleOfflineAdd 批量添加离线下载链接。
func (s *Server) handleOfflineAdd(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	var req struct {
		Urls     string `json:"urls"`
		SavePath string `json:"save_path"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	var urls []string
	for line := range strings.Lines(req.Urls) {
		if norm := normalizeMagnetURL(line); norm != "" {
			urls = append(urls, norm)
		}
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "请至少提供一条下载链接")
		return
	}

	dirID, savePath, err := s.resolveCloudDir(r, req.SavePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存目录无效: %v", err)
		return
	}
	results, err := s.Pan.AddOfflineTasks(r.Context(), urls, dirID, savePath)
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
	slog.InfoContext(r.Context(), "添加离线任务", "提交", len(urls), "成功", added, "保存路径", savePath, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "results": results})
}

// handleOfflineTorrent 添加 BT 种子离线任务。
func (s *Server) handleOfflineTorrent(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(torrentMaxSize); err != nil {
		writeErr(w, http.StatusBadRequest, "解析上传数据失败: %v", err)
		return
	}
	file, hdr, err := r.FormFile("torrent")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "未收到种子文件")
		return
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			slog.DebugContext(r.Context(), "关闭种子文件失败", "错误", cerr)
		}
	}()
	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "读取种子文件失败")
		return
	}
	savePath := normalizeSavePath(r.FormValue("save_path"), s.Conf.Settings.OfflineDir)
	result, err := drive.AddTorrentFromData(r.Context(), s.Pan, data, hdr.Filename, savePath)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加种子任务失败: %v", err)
		return
	}
	added := 0
	if result.State {
		added = 1
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"added":   added,
		"results": []drive.OfflineAddResult{*result},
	})
}

// handleOfflineDelete 删除离线任务。
func (s *Server) handleOfflineDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InfoHash    string `json:"info_hash"`
		DeleteFiles bool   `json:"delete_files"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if req.InfoHash == "" {
		writeErr(w, http.StatusBadRequest, "缺少 info_hash")
		return
	}
	if err := s.Pan.DeleteTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}

// handleOfflineClear 批量清除任务（flag 0-5）。
func (s *Server) handleOfflineClear(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Flag int `json:"flag"`
	}
	if err := readJSON(w, r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式错误: %v", err)
		return
	}
	if req.Flag < 0 || req.Flag > 5 {
		writeErr(w, http.StatusBadRequest, "flag 取值范围 0-5")
		return
	}
	if err := s.Pan.ClearTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	writeOK(w, http.StatusOK)
}

// normalizeSavePath 归一离线下载保存目录：去首尾空格与斜杠，空则回退默认，再空则云端根 "/"。
func normalizeSavePath(raw, fallback string) string {
	p := strings.Trim(strings.TrimSpace(raw), "/")
	if p == "" {
		p = fallback
	}
	p = strings.Trim(strings.TrimSpace(p), "/")
	if p == "" {
		p = "/"
	}
	return p
}

// resolveCloudDir 解析离线下载保存目录：留空依次回退全局默认目录 → 云端根 "/"，返回 (FID, 路径, 错误)。
func (s *Server) resolveCloudDir(r *http.Request, savePath string) (string, string, error) {
	savePath = normalizeSavePath(savePath, s.Conf.Settings.OfflineDir)
	info, err := s.Pan.GetDirInfo(r.Context(), savePath)
	if err != nil {
		return "", savePath, err
	}
	return info.Fid, savePath, nil
}

// normalizeMagnetURL 规范化磁链输入：漏写前缀时自动补全。
func normalizeMagnetURL(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "magnet:") || strings.Contains(s, "://") {
		return s
	}
	hash := strings.TrimPrefix(strings.TrimPrefix(s, "btih:"), "urn:btih:")
	if isBTHash(hash) {
		return "magnet:?xt=urn:btih:" + hash
	}
	return s
}

// isBTHash 判断是否为合法 BT info_hash（40 位 hex 或 32 位 base32）。
func isBTHash(s string) bool {
	if len(s) == 40 {
		for _, c := range s {
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
		return true
	}
	if len(s) == 32 {
		for _, c := range s {
			if !strings.ContainsRune("0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ234567", c) {
				return false
			}
		}
		return true
	}
	return false
}
