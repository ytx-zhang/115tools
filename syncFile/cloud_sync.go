package syncFile

import (
	"115tools/db"
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

func (s *SyncFile) StartCloudSync(parentCtx context.Context) {
	if !s.Cloud.Stats.running.CompareAndSwap(false, true) {
		return
	}
	start := time.Now()
	defer func() {
		slog.Info("云端文件同步完成", "总数", s.Cloud.Stats.total.Load(), "耗时", time.Since(start))
		s.Cloud.Stats.running.Store(false)
		s.Cloud.cancelFunc(nil)
	}()
	s.Cloud.Stats.Reset()
	ctx, cancel := context.WithCancelCause(parentCtx)
	s.Cloud.cancelFunc = cancel
	slog.Info("开始同步云端文件...")

	// 致命错误已由 VisitFile/EnterDir 内的 failLog 记录，这里显式忽略返回值。
	_ = s.WalkCloud(ctx, s.paths.SyncPath, s.paths.SyncFid, CloudVisitor{
		SkipByCount: true,
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if s.db.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					failLog(&s.Cloud.Stats, path, "创建目录失败", err)
					return false, nil
				}
				s.db.SaveRecord(path, fid, db.SizeDir)
				slog.Info("创建本地目录", "路径", path)
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e CloudEntry) error {
			savePath, saveSize := processCloudFile(path, e)
			stats := &s.Cloud.Stats

			dbFid := s.db.GetFid(savePath)
			if dbFid != "" && dbFid != fid {
				if err := s.api.DeleteFile(ctx, fid); err != nil {
					failLog(stats, savePath, "清理云端冗余项失败", err)
				} else {
					slog.Info("删除云端冗余项", "路径", savePath, "云端FID", fid)
				}
				return nil
			}
			if dbFid != "" {
				return nil
			}
			stats.total.Add(1)
			if err := s.fetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo, stats); err != nil {
				return nil
			}
			s.db.SaveRecord(savePath, fid, saveSize)
			stats.completed.Add(1)
			return nil
		},
	}, nil)
}

func (s *SyncFile) StopCloudSync() {
	if s.Cloud.Stats.running.Load() && s.Cloud.cancelFunc != nil {
		s.Cloud.cancelFunc(fmt.Errorf("用户请求停止同步"))
	}
}
