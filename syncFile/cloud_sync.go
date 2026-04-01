package syncFile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (s *SyncFile) StartCloudSync(parentCtx context.Context) {
	if s.CloudSyncStats.running.CompareAndSwap(false, true) {
		s.cloudSyncLocker.Lock()
		start := time.Now()
		defer func() {
			slog.Info("云端文件同步完成", "总数", s.CloudSyncStats.total.Load(), "耗时", time.Since(start))
			s.cloudSyncLocker.Unlock()
			s.CloudSyncStats.running.Store(false)
			s.cloudCancelFunc(nil)
		}()
		s.CloudSyncStats.Reset()
		var ctx context.Context
		ctx, s.cloudCancelFunc = context.WithCancelCause(parentCtx)
		slog.Info("开始同步云端文件...")
		var wg sync.WaitGroup
		wg.Go(func() {
			s.cloudSync(ctx, s.syncPath, s.syncFid, &wg)
		})
		wg.Wait()
	}
}

func (s *SyncFile) StopCloudSync() {
	if s.CloudSyncStats.running.Load() && s.cloudCancelFunc != nil {
		s.cloudCancelFunc(fmt.Errorf("用户请求停止同步"))
	}
}

func (s *SyncFile) cloudSync(ctx context.Context, cloudPath string, cloudFID string, wg *sync.WaitGroup) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}

	info, err := s.api.GetDirInfo(ctx, cloudPath)
	if err != nil {
		slog.Error("获取文件夹信息失败", "路径", cloudPath, "错误信息", err)
		return
	}
	cloudTotal := info.FileCount + info.FolderCount
	dbTotal := s.db.GetTotalCount(cloudPath)
	if cloudTotal == dbTotal {
		return
	}

	items, err := s.api.GetFileList(ctx, cloudFID)
	if err != nil {
		slog.Error("获取文件列表失败", "路径", cloudPath, "错误信息", err)
		return
	}

	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return
		}
		fid := item.Fid
		pickCode := item.PickCode

		fullPath := filepath.Join(cloudPath, item.Name)
		if item.IsDir {
			if s.db.GetFid(fullPath) == "" {
				_ = os.MkdirAll(fullPath, 0755)
				s.db.SaveRecord(fullPath, fid, -1)
			}
			wg.Go(func() {
				s.cloudSync(ctx, fullPath, fid, wg)
			})
			continue
		}

		isStrm := item.IsVideo
		savedPath := fullPath
		saveSize := item.Size

		if isStrm {
			savedPath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
			saveSize = time.Now().Unix()
		}

		dbFid := s.db.GetFid(savedPath)
		if dbFid != "" && dbFid != fid {
			if err := s.api.DeleteFile(ctx, fid); err != nil {
				s.CloudSyncStats.markFailed(fmt.Sprintf("[%s] 清理云端冗余项失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
			}
			continue
		}
		if dbFid != "" {
			continue
		}
		s.CloudSyncStats.total.Add(1)
		if isStrm {
			if err := s.saveStrmFile(pickCode, fid, savedPath); err != nil {

				slog.Error("创建strm文件失败", "文件", savedPath, "错误", err)
				s.CloudSyncStats.markFailed(fmt.Sprintf("[%s] 创建strm文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				continue
			}
			slog.Debug("新增STRM文件", "文件", savedPath)
		} else {
			if err := s.downloadFile(ctx, pickCode, savedPath); err != nil {
				slog.Error("下载文件失败", "文件", savedPath, "错误", err)
				s.CloudSyncStats.markFailed(fmt.Sprintf("[%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				continue
			}
			slog.Debug("下载文件成功", "文件", savedPath)
		}
		s.db.SaveRecord(savedPath, fid, saveSize)
		s.CloudSyncStats.completed.Add(1)
	}
}
