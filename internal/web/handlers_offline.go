package web

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 离线下载 ────

const _torrentMaxSize = 10 << 20

func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, err := strconv.Atoi(r.URL.Query().Get("page"))
	if err != nil {
		page = 0 // 缺失/非法 → 第 0 页
	}
	list, err := s.App.OfflineTaskList(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.App.OfflineQuotaInfo(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取配额失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

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

	savePath := strings.TrimSpace(req.SavePath)
	if savePath == "" {
		savePath = strings.TrimSpace(s.App.ConfigSnapshot().StrmPath)
	}
	dirID, err := s.App.ResolveCloudDir(r.Context(), savePath)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "保存目录无效: %v", err)
		return
	}

	results, err := s.App.AddOfflineTasks(r.Context(), urls, dirID, savePath)
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
	logs.Info(logs.ModuleSystem, "添加离线任务", "提交", len(urls), "成功", added, "保存路径", savePath, "耗时", time.Since(t0))
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "results": results})
}

// normalizeMagnetURL 规范化单个磁链/下载链接输入（web 层输入校验的一部分，
// 不在 drive 侧做）：用户漏写 magnet:?xt=urn:btih: 前缀、只贴 info_hash
// （40 位 hex 或 32 位 base32）时自动补全；已是完整链接则原样返回；空行返回空串。
func normalizeMagnetURL(line string) string {
	s := strings.TrimSpace(line)
	if s == "" {
		return s
	}
	if strings.HasPrefix(s, "magnet:") || strings.Contains(s, "://") {
		return s
	}
	hash := strings.TrimPrefix(s, "btih:")
	hash = strings.TrimPrefix(hash, "urn:btih:")
	if isBTHash(hash) {
		return "magnet:?xt=urn:btih:" + hash
	}
	return s
}

// isBTHash 判断字符串是否为合法的 BT info_hash：40 位 hex 或 32 位 base32（大小写不敏感）。
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

func (s *Server) handleOfflineTorrent(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
	if err := r.ParseMultipartForm(_torrentMaxSize); err != nil {
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
			logs.Debug(logs.ModuleSystem, "关闭种子文件失败", "错误", cerr)
		}
	}()

	data, err := io.ReadAll(file)
	if err != nil || len(data) == 0 {
		writeErr(w, http.StatusBadRequest, "读取种子文件失败")
		return
	}

	cfg := s.App.ConfigSnapshot()

	savePath := strings.TrimSpace(r.FormValue("save_path"))
	if savePath == "" {
		savePath = strings.TrimSpace(cfg.StrmPath)
	}
	savePath = strings.Trim(savePath, "/")
	if savePath == "" {
		savePath = "/"
	}

	result, err := s.App.AddTorrentTask(r.Context(), data, hdr.Filename, savePath)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "添加种子任务失败: %v", err)
		return
	}

	logs.Info(logs.ModuleSystem, "添加种子任务",
		"文件名", hdr.Filename, "大小", len(data),
		"info_hash", result.InfoHash, "保存路径", savePath, "成功", result.State, "耗时", time.Since(t0))
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

func (s *Server) handleOfflineDelete(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
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
	if err := s.App.DeleteOfflineTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "删除离线任务", "info_hash", req.InfoHash, "删除源文件", req.DeleteFiles, "耗时", time.Since(t0))
	writeOK(w, http.StatusOK)
}

func (s *Server) handleOfflineClear(w http.ResponseWriter, r *http.Request) {
	t0 := time.Now()
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
	if err := s.App.ClearOfflineTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	logs.Info(logs.ModuleSystem, "批量清除任务", "flag", req.Flag, "耗时", time.Since(t0))
	writeOK(w, http.StatusOK)
}
