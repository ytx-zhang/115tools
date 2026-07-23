package syncFile

import (
	"115tools/db"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

func (s *SyncFile) initRoot(parentCtx context.Context) error {
	if err := context.Cause(parentCtx); err != nil {
		slog.Warn("[任务中止] 初始化同步", "错误信息", err)
		return err
	}
	var ctx context.Context
	ctx, cancel := context.WithCancelCause(parentCtx)
	defer cancel(nil)
	stopWithErr := func(err error) {
		cancel(err)
	}
	dbFid := s.db.GetFid(s.paths.SyncPath)
	if dbFid != "" {
		s.paths.SyncFid = dbFid
		return nil
	}
	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	info, err := s.api.GetDirInfo(ctx, s.paths.SyncPath)
	if err != nil {
		return err
	}
	s.paths.SyncFid = info.Fid
	s.db.SaveRecord(s.paths.SyncPath, s.paths.SyncFid, db.SizeDir)
	// 批量写入器：cloudScan 大批量写入合并为少量事务；遍历成功后落盘。
	// WalkCloud 内部自管目录递归并发，此处直接同步调用即可。
	// 注意：仅在扫描成功时 Flush——若扫描失败，下方会 BatchClearPaths 清理 DB，
	// 此时若仍执行 Flush 会把半成品缓冲写回，破坏清理。
	writer := db.NewBatchWriter(s.db, 0)
	var scanErr error
	defer func() {
		if scanErr == nil {
			writer.Flush()
		}
	}()

	scanErr = s.WalkCloud(ctx, s.paths.SyncPath, s.paths.SyncFid, CloudVisitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			writer.Put(path, fid, db.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, e CloudEntry) error {
			saveSize := e.Size
			if e.IsVideo {
				path = strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
				saveSize = 0
				if info, err := os.Stat(path); err == nil {
					if _, localFid := extractPickcode(path); localFid == fid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
			writer.Put(path, fid, saveSize)
			return nil
		},
	}, stopWithErr)
	if scanErr != nil {
		cancel(scanErr)
	}

	if err := context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			s.db.BatchClearPaths([]string{s.paths.SyncPath})
		}
		return err
	}

	slog.Info("[初始化] 云端数据库初始化完成")
	return nil
}

func (s *SyncFile) initTemp(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}
	dbFid := s.db.GetFid(s.paths.TempPath)
	if dbFid != "" {
		s.paths.TempFid = dbFid
		return nil
	}
	info, err := s.api.GetDirInfo(ctx, s.paths.TempPath)
	if err != nil {
		return err
	}
	s.paths.TempFid = info.Fid
	s.db.SaveRecord(s.paths.TempPath, s.paths.TempFid, db.SizeDir)
	return nil
}

func (s *SyncFile) cronSync(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Debug("触发定时全量同步任务")
			s.addLocalSyncTask(s.paths.SyncPath)
			s.StartCloudSync(ctx)
		case <-ctx.Done():
			return
		}
	}
}
