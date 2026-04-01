package syncFile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"
)

func (s *SyncFile) addLocalSyncTask(path string) {
	s.localSyncMu.Lock()
	skip := false
	for existing := range s.localSyncPaths {
		if existing == path || strings.HasPrefix(path, existing+"/") {
			skip = true
			break
		}
		if strings.HasPrefix(existing, path+"/") {
			delete(s.localSyncPaths, existing)
		}
	}
	if !skip {
		s.localSyncPaths[path] = struct{}{}
	}
	s.localSyncMu.Unlock()

	select {
	case s.localSyncChan <- struct{}{}:
	default:
	}
}

func (s *SyncFile) GetlocalSyncTasks() []string {
	s.localSyncMu.Lock()
	defer s.localSyncMu.Unlock()

	if len(s.localSyncPaths) == 0 {
		return nil
	}

	result := make([]string, 0, len(s.localSyncPaths))
	for path := range s.localSyncPaths {
		result = append(result, path)
	}
	s.localSyncPaths = make(map[string]struct{})
	return result
}

func (s *SyncFile) startLocalSyncWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.Info("本地文件同步Worker已退出")
			return
		case _, ok := <-s.localSyncChan:
			if !ok {
				return
			}
			paths := s.GetlocalSyncTasks()
			for _, path := range paths {
				func() {
					s.cloudSyncLocker.RLock()
					defer s.cloudSyncLocker.RUnlock()
					if ctx.Err() != nil {
						return
					}
					fid := s.db.GetFid(path)
					if fid != "" {
						if _, err := os.Stat(path); err == nil {
							s.localSync(ctx, path, fid)
						}
					}
				}()
			}
		}
	}
}
func (s *SyncFile) watchSyncPath(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(s.syncPath),
		fswatcher.WithEventBatching(1*time.Second),
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", s.syncPath)
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

func (s *SyncFile) localSync(ctx context.Context, currentPath string, currentFid string) {
	start := time.Now()
	var uploadPaths []string
	defer func() {
		slog.Info("本地文件同步完成", "目录", currentPath, "耗时", time.Since(start), "总数", len(uploadPaths))
	}()
	s.localScan(ctx, currentPath, currentFid, &uploadPaths)
	if len(uploadPaths) == 0 {
		return
	}
	uploadChan := make(chan string, len(uploadPaths))
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			for fPath := range uploadChan {
				if err := ctx.Err(); err != nil {
					return
				}
				fileInfo, err := os.Stat(fPath)
				if err != nil {
					slog.Warn("同步的文件不存在", "文件", fPath)
					continue
				}

				cid := s.db.GetFid(filepath.Dir(fPath))
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
		})
	}
	for _, fPath := range uploadPaths {
		uploadChan <- fPath
	}
	close(uploadChan)
	wg.Wait()
}

func (s *SyncFile) localScan(ctx context.Context, currentPath string, currentFid string, uploadTasks *[]string) {
	if err := ctx.Err(); err != nil {
		return
	}
	localFlieList, err := os.ReadDir(currentPath)
	if err != nil {
		slog.Error(err.Error())
		return
	}
	localFiles := make(map[string]os.DirEntry, len(localFlieList))
	for _, file := range localFlieList {
		localFiles[file.Name()] = file
	}
	var deleteFilePaths []string
	var toUploadTasks []string

	s.db.ScanChildren(ctx, currentPath, func(name string, dbVal string) {
		if err := ctx.Err(); err != nil {
			return
		}
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)
		//云端存在,本地不存在
		if !exists {
			deleteFilePaths = append(deleteFilePaths, fullPath)
			return
		}

		delete(localFiles, name)

		var dbFid string
		var dbSize int64 = -2
		if before, after, ok := strings.Cut(dbVal, "|"); ok {
			dbFid = before
			dbSize, _ = strconv.ParseInt(after, 10, 64)
		}

		if localFile.IsDir() {
			if dbSize == -1 {
				s.localScan(ctx, fullPath, dbFid, uploadTasks)
			}
			return
		}

		info, err := localFile.Info()
		if err != nil {
			slog.Error(err.Error())
			return
		}

		isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
		var fileSize int64
		if isStrm {
			fileSize = info.ModTime().Unix()
		} else {
			fileSize = info.Size()
		}
		//文件大小不匹配的文件
		if dbSize != -1 && fileSize != dbSize {
			deleteFilePaths = append(deleteFilePaths, fullPath)
			toUploadTasks = append(toUploadTasks, fullPath)
		}
	})
	//批量删除文件
	if err := s.cloudCleanTask(ctx, deleteFilePaths, currentPath); err == nil {
		//上传任务
		*uploadTasks = append(*uploadTasks, toUploadTasks...)
	} else {
		slog.Error(err.Error())
	}

	//本地新增
	for fileName, localFile := range localFiles {
		if err := ctx.Err(); err != nil {
			return
		}
		fullPath := filepath.Join(currentPath, fileName)

		if localFile.IsDir() {
			fid, err := s.addCloudFolder(ctx, currentFid, fileName, fullPath)
			if err == nil {
				s.localScan(ctx, fullPath, fid, uploadTasks)
			} else {
				slog.Error(err.Error())
			}
			continue
		}
		*uploadTasks = append(*uploadTasks, fullPath)
	}
}
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
			if err := s.api.MoveFile(ctx, dbFid, s.tempFid); err != nil {
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
		info, err := s.api.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = info.Fid
	}

	if err := s.api.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	fileName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := s.api.UpdateFile(ctx, fid, fileName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端视频改名失败: %w", fPath, err)
	}

	trueName := fileName + filepath.Ext(newName)
	if newName != trueName {
		_, err = s.api.UpdateFile(ctx, fid, trueName)
		if err != nil {
			return fmt.Errorf("[%s]: 云端视频二次改名失败: %w", fPath, err)
		}
	}
	if err := s.saveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	s.db.SaveRecord(fPath, fid, time.Now().Unix())
	return nil
}
