package addStrm

import (
	"115tools/config"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	strmPath   string
	tempPath   string
	rootFids   []string
	cancelFunc context.CancelCauseFunc
	sem        = make(chan struct{}, 5)
)

func StartAddStrm(parentCtx context.Context) {
	if stats.running.CompareAndSwap(false, true) {
		start := time.Now()
		var ctx context.Context
		ctx, cancelFunc = context.WithCancelCause(parentCtx)

		defer func() {
			stats.running.Store(false)
			cancelFunc(nil)
			slog.Info("生成strm任务结束", "总数", stats.total.Load(), "耗时", time.Since(start))
		}()

		stats.Reset()
		conf := config.Get()
		strmPath = conf.StrmPath
		tempPath = conf.TempPath
		rootFids = nil

		slog.Info("开始生成strm文件...")

		startFid, _, _, err := open115.FolderInfo(ctx, strmPath)
		if err != nil {
			slog.Error("无法获取起始目录id", "错误信息", err)
			return
		}

		var wg sync.WaitGroup
		wg.Go(func() {
			runAddStrmTask(ctx, startFid, strmPath, &wg)
		})
		wg.Wait()

		// 全部扫描并下载完成后，执行移动逻辑
		if err := context.Cause(ctx); err == nil && len(stats.failedErrors) == 0 && len(rootFids) > 0 {
			performMoveFiles(ctx)
		}
	}
}

func StopAddStrm() {
	if stats.running.Load() && cancelFunc != nil {
		cancelFunc(fmt.Errorf("用户请求停止任务"))
	}
}

func runAddStrmTask(ctx context.Context, fid string, currentPath string, wg *sync.WaitGroup) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}

	list, err := open115.FileList(ctx, fid)
	if err != nil {
		slog.Error("获取云端列表失败", "路径", currentPath, "错误信息", err)
		return
	}

	for _, item := range list {
		// 每次迭代检查停止信号
		if err := context.Cause(ctx); err != nil {
			return
		}

		itemFid := item.Get("fid").String()
		fileName := item.Get("fn").String()
		fullPath := filepath.Join(currentPath, fileName)

		// 记录根目录下的 FID 供后续移动使用
		if currentPath == strmPath {
			rootFids = append(rootFids, itemFid)
		}

		// 情况 A: 文件夹 -> 递归
		if item.Get("fc").String() == "0" {
			_ = os.MkdirAll(fullPath, 0755)
			wg.Go(func() {
				runAddStrmTask(ctx, itemFid, fullPath, wg)
			})
			continue
		}

		// 情况 B: 文件 -> 判断并执行下载/生成 strm
		isStrm := item.Get("isv").Int() == 1
		finalPath := fullPath
		if isStrm {
			finalPath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
		}

		// 检查本地是否存在
		if _, err := os.Stat(finalPath); err == nil {
			continue
		}

		stats.total.Add(1)
		pc := item.Get("pc").String()

		if isStrm {
			if err := open115.SaveStrmFile(pc, fid, finalPath); err != nil {
				slog.Error("创建strm文件失败", "文件", finalPath, "错误", err)
				markFailed(fmt.Sprintf("[%s] 创建strm文件失败: %s (%v)", time.Now().Format("15:04"), finalPath, err))
				continue
			}
			slog.Info("新增STRM文件", "路径", finalPath)
		} else {
			if err := open115.DownloadFile(ctx, pc, finalPath); err != nil {
				slog.Error("下载文件失败", "文件", finalPath, "错误", err)
				markFailed(fmt.Sprintf("[%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), finalPath, err))
				continue
			}
			slog.Info("下载文件成功", "文件", finalPath)
		}
	}
}

func performMoveFiles(ctx context.Context) {
	targetCID, _, _, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		slog.Error("无法获取移动目标目录", "错误信息", err)
		return
	}

	fidsStr := strings.Join(rootFids, ",")
	count := len(rootFids)

	if count == 0 {
		return
	}

	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		markFailed(fmt.Sprintf("移动文件失败: %v", err))
	} else {
		slog.Info("移动文件至 TempPath", "文件数量", count)
	}
}
