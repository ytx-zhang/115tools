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

func (s *SyncFile) StartAddStrm(parentCtx context.Context) {
	if s.AddStrmStats.running.CompareAndSwap(false, true) {
		start := time.Now()
		var ctx context.Context
		ctx, s.addStrmCancelFunc = context.WithCancelCause(parentCtx)

		defer func() {
			s.AddStrmStats.running.Store(false)
			s.addStrmCancelFunc(nil)
			slog.Info("生成strm任务结束", "总数", s.AddStrmStats.total.Load(), "耗时", time.Since(start))
		}()

		s.AddStrmStats.Reset()
		s.moveFids = nil

		slog.Info("开始生成strm文件...")

		info, err := s.api.GetDirInfo(ctx, s.strmPath)
		if err != nil {
			slog.Error("无法获取起始目录id", "错误信息", err)
			return
		}
		var wg sync.WaitGroup
		wg.Go(func() {
			s.runAddStrmTask(ctx, info.Fid, s.strmPath, &wg)
		})
		wg.Wait()
		if err := context.Cause(ctx); err != nil {
			slog.Error("生成strm任务被取消", "取消信息", err)
			return
		}
		if len(s.AddStrmStats.failedErrors) == 0 && len(s.moveFids) > 0 {
			s.moveStrmPathFiles(ctx, s.moveFids)
		}
	}
}

func (s *SyncFile) StopAddStrm() {
	if s.AddStrmStats.running.Load() && s.addStrmCancelFunc != nil {
		s.addStrmCancelFunc(fmt.Errorf("用户请求停止任务"))
	}
}

func (s *SyncFile) runAddStrmTask(ctx context.Context, currentFid string, currentPath string, wg *sync.WaitGroup) {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return
	}

	flieList, err := s.api.GetFileList(ctx, currentFid)
	if err != nil {
		slog.Error("获取云端列表失败", "路径", currentPath, "错误信息", err)
		return
	}

	for _, cloudFile := range flieList {
		// 每次迭代检查停止信号
		if err := context.Cause(ctx); err != nil {
			return
		}

		fid := cloudFile.Fid
		name := cloudFile.Name
		savePath := filepath.Join(currentPath, name)

		// 记录根目录下的 FID 供后续移动使用
		if currentPath == s.strmPath {
			s.moveFids = append(s.moveFids, fid)
		}

		// 情况 A: 文件夹 -> 递归
		if cloudFile.IsDir {
			_ = os.MkdirAll(savePath, 0755)
			wg.Go(func() {
				s.runAddStrmTask(ctx, fid, savePath, wg)
			})
			continue
		}

		// 情况 B: 文件 -> 判断并执行下载/生成 strm
		isStrm := cloudFile.IsVideo
		if isStrm {
			savePath = strings.TrimSuffix(savePath, filepath.Ext(savePath)) + ".strm"
		}

		// 检查本地是否存在
		if _, err := os.Stat(savePath); err == nil {
			continue
		}

		s.AddStrmStats.total.Add(1)
		pickCode := cloudFile.PickCode

		if isStrm {
			if err := s.saveStrmFile(pickCode, fid, savePath); err != nil {
				slog.Error("创建strm文件失败", "文件", savePath, "错误", err)
				s.AddStrmStats.markFailed(fmt.Sprintf("[%s] 创建strm文件失败: %s (%v)", time.Now().Format("15:04"), savePath, err))
				continue
			}
			slog.Debug("新增STRM文件", "路径", savePath)
		} else {
			if err := s.downloadFile(ctx, pickCode, savePath); err != nil {
				slog.Error("下载文件失败", "文件", savePath, "错误", err)
				s.AddStrmStats.markFailed(fmt.Sprintf("[%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), savePath, err))
				continue
			}
			slog.Debug("下载文件成功", "文件", savePath)
		}
		s.AddStrmStats.completed.Add(1)
	}
}

func (s *SyncFile) moveStrmPathFiles(ctx context.Context, paths []string) {
	fidsStr := strings.Join(paths, ",")
	count := len(paths)
	if err := s.api.MoveFile(ctx, fidsStr, s.tempFid); err != nil {
		slog.Error("移动文件失败", "目录", "TempPath", "错误", err)
		s.AddStrmStats.markFailed(fmt.Sprintf("移动文件至 TempPath 失败: %v", err))
	} else {
		slog.Debug("移动文件至 TempPath", "文件数量", count)
	}
}
