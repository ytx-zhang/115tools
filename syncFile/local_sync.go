package syncFile

import (
	"115tools/db"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgtdi/fswatcher"
)

func (s *SyncFile) addLocalSyncTask(path string) {
	s.local.mu.Lock()
	skip := false
	for existing := range s.local.paths {
		if existing == path || strings.HasPrefix(path, existing+"/") {
			skip = true
			break
		}
		if strings.HasPrefix(existing, path+"/") {
			delete(s.local.paths, existing)
		}
	}
	if !skip {
		s.local.paths[path] = struct{}{}
	}
	s.local.mu.Unlock()

	select {
	case s.local.ch <- struct{}{}:
	default:
	}
}

func (s *SyncFile) getLocalSyncTasks() []string {
	s.local.mu.Lock()
	defer s.local.mu.Unlock()

	if len(s.local.paths) == 0 {
		return nil
	}
	result := make([]string, 0, len(s.local.paths))
	for path := range s.local.paths {
		result = append(result, path)
	}
	s.local.paths = make(map[string]struct{})
	return result
}

func (s *SyncFile) startLocalSyncWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.Info("本地文件同步Worker已退出")
			return
		case _, ok := <-s.local.ch:
			if !ok {
				return
			}
			paths := s.getLocalSyncTasks()
			for _, path := range paths {
				s.Cloud.locker.RLock()
				if ctx.Err() != nil {
					s.Cloud.locker.RUnlock()
					return
				}
				fid := s.db.GetFid(path)
				s.Cloud.locker.RUnlock()

				if fid != "" {
					if _, err := os.Stat(path); err == nil {
						s.localSync(ctx, path, fid)
					}
				}
			}
		}
	}
}

func (s *SyncFile) watchSyncPath(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(s.paths.SyncPath),
		fswatcher.WithCooldown(1*time.Second),
		fswatcher.WithBufferSize(40960),
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", s.paths.SyncPath)
	for {
		select {
		case <-ctx.Done():
			slog.Info("文件监听器已退出")
			return
		case event, ok := <-watcher.Events():
			if !ok {
				return
			}
			dir := filepath.Clean(filepath.Dir(event.Path))
			s.addLocalSyncTask(dir)
		}
	}
}

// ──── 本地同步主流程 ────

func (s *SyncFile) localSync(ctx context.Context, currentPath string, currentFid string) {
	start := time.Now()
	uploadPaths := s.localScan(ctx, currentPath, currentFid)
	if len(uploadPaths) == 0 {
		return
	}
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			return
		}
		cid := s.db.GetFid(filepath.Dir(fPath))
		if cid == "" {
			slog.Warn("无法获取父目录FID", "文件", fPath)
			continue
		}
		s.uploadOneFile(ctx, cid, fPath)
	}
	slog.Info("本地文件同步完成", "目录", currentPath, "耗时", time.Since(start), "上传", len(uploadPaths))
}

// ──── 目录扫描：对比 DB 与本地文件系统 ────

func (s *SyncFile) localScan(ctx context.Context, currentPath, currentFid string) []string {
	slog.Debug("扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() { slog.Debug("本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start)) }()

	if err := ctx.Err(); err != nil {
		return nil
	}

	localFiles, err := readLocalDir(currentPath)
	if err != nil {
		slog.Error("读取本地目录失败", "路径", currentPath, "错误", err)
		return nil
	}

	var deletes []string
	var uploads []string

	// 遍历 DB 子项，与本地对比
	s.db.ScanChildren(ctx, currentPath, func(name string, dbFid string, dbSize int64) {
		if err := ctx.Err(); err != nil {
			return
		}
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			deletes = append(deletes, fullPath) // 云端存在，本地不存在 → 删除
			return
		}
		delete(localFiles, name)

		if localFile.IsDir() {
			if dbSize == db.SizeDir {
				uploads = append(uploads, s.localScan(ctx, fullPath, dbFid)...)
			}
			return
		}

		// 对比文件是否变化
		localSize := compareLocalFile(s.db, fullPath, name, dbFid, dbSize)
		if localSize >= 0 && localSize != dbSize {
			deletes = append(deletes, fullPath)
			uploads = append(uploads, fullPath)
		}
	})

	// 云删除与 DB 清理
	if err := s.cloudCleanTask(ctx, deletes, currentPath); err != nil {
		slog.Error("云端删除失败", "目录", currentPath, "错误", err)
	}

	// 本地新增项（不在 DB 中的）
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return uploads
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			fid, err := s.addCloudFolder(ctx, currentFid, name, fullPath)
			if err != nil {
				slog.Error("创建云端目录失败", "路径", fullPath, "错误", err)
				continue
			}
			uploads = append(uploads, s.localScan(ctx, fullPath, fid)...)
		} else {
			uploads = append(uploads, fullPath)
		}
	}

	return uploads
}

// ──── 文件对比 ────

// compareLocalFile 返回本地文件用于和 DB 对比的大小值。
// .strm 文件用修改时间（Unix 秒）代替文件大小；普通文件直接返回字节数。
// 返回 -1 表示文件不可读。
func compareLocalFile(boltDB *db.DB, fullPath, name, dbFid string, dbSize int64) int64 {
	info, err := os.Stat(fullPath)
	if err != nil {
		return -1
	}
	isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
	if !isStrm {
		return info.Size()
	}
	// .strm 文件：若仅修改时间变化但 pickcode 中 fid 匹配，视为本地已知变更，
	// 更新 DB 记录（不触发重新上传）
	localSize := info.ModTime().Unix()
	if localSize != dbSize {
		_, fid := extractPickcode(fullPath)
		if fid == dbFid {
			boltDB.SaveRecord(fullPath, fid, localSize)
			return localSize
		}
	}
	return localSize
}

// readLocalDir 读取目录内容到 map，key 为文件名。
func readLocalDir(path string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		m[e.Name()] = e
	}
	return m, nil
}

// ──── 单文件上传 ────

func (s *SyncFile) uploadOneFile(ctx context.Context, cid, fPath string) {
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		slog.Warn("同步的文件不存在", "文件", fPath)
		return
	}

	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	if isStrm {
		err = s.upStrmTask(ctx, cid, fPath)
	} else {
		err = s.upFileTask(ctx, cid, fPath, fileInfo)
	}
	if err != nil {
		slog.Error("同步失败", "文件", fPath, "错误", err)
	} else {
		slog.Debug("同步成功", "文件", fPath)
	}
}

// ──── 上传任务实现 ────

func (s *SyncFile) upFileTask(ctx context.Context, cid, fPath string, fileInfo os.FileInfo) error {
	info, err := s.api.UploadFile(ctx, fPath, cid, "", "")
	if err != nil {
		return err
	}
	cloudFid := info.Fid
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	if checkVideo(ext, size) {
		savePath = strings.TrimSuffix(fPath, ext) + ".strm"
		dbFid := s.db.GetFid(savePath)
		if dbFid != "" {
			if err := s.api.MoveFile(ctx, dbFid, s.paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := s.saveStrmFile(info.PickCode, cloudFid, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
		size = time.Now().Unix()
	}
	s.db.SaveRecord(savePath, cloudFid, size)
	return nil
}

func (s *SyncFile) upStrmTask(ctx context.Context, cid, fPath string) error {
	pickcode, fid := extractPickcode(fPath)
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		info, err := s.api.GetDownloadUrl(ctx, pickcode, "115tools")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = info.Fid
	}

	if err := s.api.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	// 恢复云端文件名（上传后名称为 pickcode，需改回原名）
	origName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := s.api.UpdateFile(ctx, fid, origName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端改名失败: %w", fPath, err)
	}
	// UpdateFile 可能只改了文件名没改扩展名，补齐
	if newName != origName+filepath.Ext(newName) {
		if _, err := s.api.UpdateFile(ctx, fid, origName+filepath.Ext(newName)); err != nil {
			return fmt.Errorf("[%s]: 云端扩展名修复失败: %w", fPath, err)
		}
	}

	if err := s.saveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	s.db.SaveRecord(fPath, fid, time.Now().Unix())
	return nil
}


