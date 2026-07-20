package syncFile

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *SyncFile) StartAddStrm(parentCtx context.Context) {
	if !s.Strm.Stats.running.CompareAndSwap(false, true) {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithCancelCause(parentCtx)
	s.Strm.cancelFunc = cancel

	defer func() {
		s.Strm.Stats.running.Store(false)
		s.Strm.cancelFunc(nil)
		slog.Info("生成strm任务结束", "总数", s.Strm.Stats.total.Load(), "耗时", time.Since(start))
	}()

	s.Strm.Stats.Reset()
	s.Strm.moveFids = nil
	slog.Info("开始生成strm文件...")

	info, err := s.api.GetDirInfo(ctx, s.paths.StrmPath)
	if err != nil {
		slog.Error("无法获取起始目录id", "错误信息", err)
		return
	}

	s.WalkCloud(ctx, s.paths.StrmPath, info.Fid, CloudVisitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if filepath.Dir(path) == s.paths.StrmPath {
				s.Strm.moveFids = append(s.Strm.moveFids, fid)
			}
			if err := os.MkdirAll(path, 0755); err != nil {
				failLog(&s.Strm.Stats, path, "创建目录失败", err)
				return false, nil
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e CloudEntry) error {
			if filepath.Dir(path) == s.paths.StrmPath {
				s.Strm.moveFids = append(s.Strm.moveFids, fid)
			}
			savePath, _ := processCloudFile(path, e)
			if _, err := os.Stat(savePath); err == nil {
				return nil
			}
			s.Strm.Stats.total.Add(1)
			if err := s.fetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo, &s.Strm.Stats); err != nil {
				return nil
			}
			s.Strm.Stats.completed.Add(1)
			return nil
		},
	}, nil)

	if err := context.Cause(ctx); err != nil {
		slog.Error("生成strm任务被取消", "取消信息", err)
		return
	}
	if len(s.Strm.Stats.failedErrors) == 0 && len(s.Strm.moveFids) > 0 {
		s.moveStrmPathFiles(ctx, s.Strm.moveFids)
	}
}

func (s *SyncFile) StopAddStrm() {
	if s.Strm.Stats.running.Load() && s.Strm.cancelFunc != nil {
		s.Strm.cancelFunc(fmt.Errorf("用户请求停止任务"))
	}
}

func (s *SyncFile) moveStrmPathFiles(ctx context.Context, paths []string) {
	fidsStr := strings.Join(paths, ",")
	count := len(paths)
	if err := s.api.MoveFile(ctx, fidsStr, s.paths.TempFid); err != nil {
		slog.Error("移动文件失败", "目录", "TempPath", "错误", err)
		s.Strm.Stats.markFailed(fmt.Sprintf("移动文件至 TempPath 失败: %v", err))
	} else {
		slog.Debug("移动文件至 TempPath", "文件数量", count)
	}
}
