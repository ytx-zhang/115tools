package syncFile

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

func (s *SyncFile) downloadFile(ctx context.Context, pickcode, localPath string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	info, err := s.api.GetDownloadUrl(ctx, pickcode, "115tools")
	if err != nil {
		return err
	}
	url := info.Url
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "115tools")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(localPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(localPath)
	if err != nil {
		return err
	}

	defer func() {
		out.Close()
		if err != nil {
			os.Remove(localPath)
		}
	}()

	_, err = io.Copy(out, resp.Body)
	return err
}
func (s *SyncFile) saveStrmFile(pickcode, fid, localPath string) error {
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", s.strmUrl, pickcode, fid)
	contentBytes := []byte(content)
	if err := os.WriteFile(localPath, contentBytes, 0644); err != nil {
		return err
	}
	return nil
}
func (s *SyncFile) addCloudFolder(ctx context.Context, currentCID, fileName, fullPath string) (string, error) {
	slog.Debug("创建云端文件夹", "路径", fullPath)
	fid, err := s.api.AddFolder(ctx, currentCID, fileName)
	if err != nil {
		return "", fmt.Errorf("[%s]: 创建云端文件夹失败: %w", fullPath, err)
	}
	s.db.SaveRecord(fullPath, fid, -1)
	return fid, nil
}
func (s *SyncFile) cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	var moveFids []string
	var deleteFids []string

	for _, fPath := range fPaths {
		fid, size := s.db.GetInfo(fPath)
		if fid == "" {
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		// 如果是 strm 文件或文件夹 (size == -1)，准备移动
		if isStrm || size == -1 {
			moveFids = append(moveFids, fid)
		} else {
			deleteFids = append(deleteFids, fid)
		}
	}

	// 1. 批量移动
	if len(moveFids) > 0 {
		fidsJoin := strings.Join(moveFids, ",")
		slog.Debug("批量移动云端文件到临时目录", "处理目录", workPath, "数量", len(moveFids))
		err := s.api.MoveFile(ctx, fidsJoin, s.tempFid)
		if err != nil {
			return fmt.Errorf("[%s]: 批量移动目录内文件失败: %w", workPath, err)
		}
	}

	// 2. 批量删除
	if len(deleteFids) > 0 {
		fidsJoin := strings.Join(deleteFids, ",")
		slog.Debug("批量删除云端文件", "处理目录", workPath, "数量", len(deleteFids))
		err := s.api.DeleteFile(ctx, fidsJoin)
		if err != nil {
			return fmt.Errorf("[%s]: 批量删除目录内文件失败: %w", workPath, err)
		}
	}

	// 3. 清理数据库记录
	s.db.BatchClearPaths(fPaths)

	return nil
}
func checkVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	switch strings.ToLower(ext) {
	case ".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv":
		return true
	}
	return false
}
func extractPickcode(fPath string) (pickcode, fid string) {
	content, err := os.ReadFile(fPath)
	if err != nil {
		return "", ""
	}
	u, err := url.Parse(strings.TrimSpace(string(content)))
	if err != nil {
		return "", ""
	}
	pickcode = u.Query().Get("pickcode")
	fid = u.Query().Get("fid")
	return
}
