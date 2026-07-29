package web

import (
	"github.com/ytx-zhang/115tools/internal/drive"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

const _torrentMaxSize = 10 << 20

func (s *Server) handleOfflineTasks(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	list, err := s.Api.OfflineTaskList(r.Context(), page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取任务列表失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleOfflineQuota(w http.ResponseWriter, r *http.Request) {
	quota, err := s.Api.OfflineQuotaInfo(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "获取配额失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, quota)
}

// handleOfflineAdd 批量添加离线下载链接。
// save_path 留空默认 strm_path，"/" 表示根目录。
func (s *Server) handleOfflineAdd(w http.ResponseWriter, r *http.Request) {
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

// handleOfflineTorrent 上传种子并添加 BT 任务（multipart/form-data）。
// save_path 空→strm_path→"/"；种子临时目录取 torrent_path→temp_path→"/"。
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

	savePath := strings.TrimSpace(r.FormValue("save_path"))
	if savePath == "" {
		savePath = strings.TrimSpace(cfg.StrmPath)
	}
	savePath = strings.Trim(savePath, "/")
	if savePath == "" {
		savePath = "/"
	}

	torrentPath := strings.TrimSpace(cfg.TorrentPath)
	if torrentPath == "" {
		torrentPath = strings.TrimSpace(cfg.TempPath)
	}
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
		"文件名", hdr.Filename, "大小", len(data),
		"info_hash", result.InfoHash, "保存路径", savePath, "成功", result.State)
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

// resolveCloudDir 把云端路径解析为目录 ID；空→strm_path，"/"→根目录("0")。
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
	if err := s.Api.DeleteOfflineTask(r.Context(), req.InfoHash, req.DeleteFiles); err != nil {
		writeErr(w, http.StatusBadGateway, "删除任务失败: %v", err)
		return
	}
	slog.Info("[离线下载] 删除任务", "info_hash", req.InfoHash, "删除源文件", req.DeleteFiles)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleOfflineClear 批量清除：0已完成 1全部 2失败 3进行中 4已完成且删文件 5全部且删文件。
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
	if err := s.Api.ClearOfflineTasks(r.Context(), req.Flag); err != nil {
		writeErr(w, http.StatusBadGateway, "清除任务失败: %v", err)
		return
	}
	slog.Info("[离线下载] 批量清除任务", "flag", req.Flag)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
