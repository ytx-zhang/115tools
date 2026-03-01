package addStrm

import (
	"115tools/config"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	strmPath   string
	tempPath   string
	rootFids   []string
	needRetry  atomic.Bool
	wg         sync.WaitGroup
	scanSem    = make(chan struct{}, 3)
	cancelFunc context.CancelCauseFunc
)

type taskStats struct {
	total        atomic.Int64
	completed    atomic.Int64
	failed       atomic.Int64
	mu           sync.Mutex
	failedErrors []string
	running      atomic.Bool
}

var stats = &taskStats{
	failedErrors: []string{},
}

func (s *taskStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.Store(0)
	s.completed.Store(0)
	s.failed.Store(0)
	s.failedErrors = s.failedErrors[:0]
}

type TaskStatsJSON struct {
	Total     int64    `json:"total"`
	Completed int64    `json:"completed"`
	Failed    int64    `json:"failed"`
	Errors    []string `json:"errors"`
	Running   bool     `json:"running"`
}

func GetStatus() TaskStatsJSON {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	return TaskStatsJSON{
		Total:     stats.total.Load(),
		Completed: stats.completed.Load(),
		Failed:    stats.failed.Load(),
		Errors:    slices.Clone(stats.failedErrors),
		Running:   stats.running.Load(),
	}
}
func markFailed(reason string) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.failed.Add(1)
	stats.failedErrors = append(stats.failedErrors, reason)
}

type task struct {
	Strm      bool
	PC        string
	FID       string
	LocalPath string
}

func StartAddStrm(parentCtx context.Context) {
	needRetry.Store(true)
	if stats.running.CompareAndSwap(false, true) {
		defer stats.running.Store(false)
		stats.Reset()
		for needRetry.Swap(false) {
			if err := context.Cause(parentCtx); err != nil {
				slog.Error("[同步] 任务中止", "错误信息", err)
				return
			}
			runAddStrm(parentCtx)
		}
	}
}
func StopAddStrm() {
	if cancelFunc != nil {
		cancelFunc(fmt.Errorf("[添加strm] 用户请求停止任务"))
	}
}
func runAddStrm(parentCtx context.Context) {
	slog.Info("[添加strm] 开始添加strm文件...")
	var ctx context.Context
	ctx, cancelFunc = context.WithCancelCause(parentCtx)
	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		slog.Info("[任务中止] 添加strm", "错误信息", context.Cause(ctx))
		return
	}
	conf := config.Get()
	strmPath = conf.StrmPath
	tempPath = conf.TempPath
	defer func() {
		cancelFunc(fmt.Errorf("[添加strm] 任务结束"))
		rootFids = nil
		slog.Info("[添加strm] 任务结束", "处理文件", stats.completed.Load(), "失败任务", stats.failed.Load())
	}()
	startFid, _, _, err := open115.FolderInfo(ctx, strmPath)
	if err != nil {
		slog.Error("[添加strm] 无法获取起始目录id", "错误信息", err)
		return
	}
	taskQueue := make(chan task, 1000)
	for range 3 {
		wg.Go(func() {
			for task := range taskQueue {
				if context.Cause(ctx) != nil {
					continue
				}
				var err error
				if task.Strm {
					err = open115.SaveStrmFile(task.PC, task.FID, task.LocalPath)
				} else {
					err = open115.DownloadFile(ctx, task.PC, task.LocalPath)
				}
				if err != nil {
					markFailed(fmt.Sprintf("[添加STRM] [%s] 失败: %s (%v)", time.Now().Format("15:04"), task.LocalPath, err))
					slog.Error("[添加STRM] 失败", "路径", task.LocalPath, "错误信息", err)
				} else {
					stats.completed.Add(1)
					slog.Info("[添加STRM] 成功", "路径", task.LocalPath)
				}
			}
		})
	}
	if err := cloudScan(ctx, startFid, strmPath, taskQueue); err != nil {
		slog.Error("[添加STRM] 云端扫描文件夹失败", "错误信息", err)
		cancelFunc(fmt.Errorf("[添加STRM] 云端扫描文件夹失败: %v", err))
	}
	close(taskQueue)
	wg.Wait()

	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] 生成strm过程中被中止", "错误信息", err)
		return
	}
	if stats.total.Load() == 0 {
		slog.Info("[添加strm] 没有需要处理的文件")
		return
	}
	if len(stats.failedErrors) == 0 && len(rootFids) > 0 {
		performMoveFiles(ctx)
	}
}

func cloudScan(ctx context.Context, fid string, currentPath string, taskQueue chan task) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	slog.Info("[添加strm] 获取云端列表", "路径", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		slog.Error("[添加strm] 获取云端列表失败", "路径", currentPath, "错误信息", err)
		return err
	}
	for _, item := range list {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		currentLocalPath := filepath.Join(currentPath, item.Fn)
		if currentPath == strmPath {
			rootFids = append(rootFids, item.Fid)
		}

		if item.Fc == "0" {
			_ = os.MkdirAll(currentLocalPath, 0755)
			cloudScan(ctx, item.Fid, currentLocalPath, taskQueue)
		} else {
			finalPath := currentLocalPath
			if item.Isv == 1 {
				finalPath = finalPath[:len(finalPath)-len(filepath.Ext(finalPath))] + ".strm"
			}
			if _, err := os.Stat(finalPath); err == nil {
				continue
			}
			taskQueue <- task{
				Strm:      item.Isv == 1,
				PC:        item.Pc,
				FID:       item.Fid,
				LocalPath: finalPath,
			}
			stats.total.Add(1)
		}
	}
	return nil
}

func performMoveFiles(ctx context.Context) {
	if err := context.Cause(ctx); err != nil {
		return
	}
	targetCID, _, _, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		slog.Error("[添加strm] 无法获取移动文件", "错误信息", err)
		return
	}
	fidsStr := strings.Join(rootFids, ",")
	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		markFailed(fmt.Sprintf("[添加STRM] [%s] 移动云盘文件失败: %v", time.Now().Format("15:04"), err))
		slog.Error("[添加strm] 移动云盘文件失败", "错误信息", err)
	} else {
		slog.Info("[添加strm] 成功移动文件至 TempPath", "文件数量", len(rootFids))
	}
}
