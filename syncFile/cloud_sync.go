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
	s.Cloud.locker.Lock()
	start := time.Now()
	defer func() {
		slog.Info("云端文件同步完成", "总数", s.Cloud.Stats.total.Load(), "耗时", time.Since(start))
		s.Cloud.locker.Unlock()
		s.Cloud.Stats.running.Store(false)
		s.Cloud.cancelFunc(nil)
	}()
	s.Cloud.Stats.Reset()
	ctx, cancel := context.WithCancelCause(parentCtx)
	s.Cloud.cancelFunc = cancel
	slog.Info("开始同步云端文件...")

	s.WalkCloud(ctx, s.paths.SyncPath, s.paths.SyncFid, CloudVisitor{
		SkipByCount: true,
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if s.db.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					failLog(&s.Cloud.Stats, path, "创建目录失败", err)
					return false, nil
				}
				s.db.SaveRecord(path, fid, db.SizeDir)
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
