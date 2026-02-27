package addStrm

import (
	"115tools/config"
	"115tools/open115"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	strmUrl    string
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

func StartAddStrm(parentCtx context.Context, mainWg *sync.WaitGroup) {
	needRetry.Store(true)
	if stats.running.CompareAndSwap(false, true) {
		mainWg.Go(func() {
			defer stats.running.Store(false)
			stats.Reset()
			for needRetry.Swap(false) {
				if err := context.Cause(parentCtx); err != nil {
					slog.Error("[同步] 任务中止", "error", err)
					return
				}
				runAddStrm(parentCtx)
			}
		})
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
		slog.Info("[任务中止] 添加strm", "error", context.Cause(ctx))
		return
	}
	conf := config.Get()
	strmPath = conf.StrmPath
	strmUrl = conf.StrmUrl
	tempPath = conf.TempPath
	defer func() {
		cancelFunc(fmt.Errorf("[添加strm] 任务结束"))
		rootFids = nil
		slog.Info("[添加strm] 任务结束", "处理文件", stats.completed.Load(), "失败任务", stats.failed.Load())
	}()
	startFid, _, _, err := open115.FolderInfo(ctx, strmPath)
	if err != nil {
		slog.Error("[添加strm] 无法获取起始目录id", "error", err)
		return
	}
	taskQueue := make(chan task, 1000)
	for range 3 {
		wg.Go(func() {
			for task := range taskQueue {
				if context.Cause(ctx) != nil {
					continue
				}
				current := stats.completed.Add(1)
				total := stats.total.Load()
				var err error
				if task.Strm {
					err = doCreateStrm(ctx, task)
				} else {
					err = doDownloadFile(ctx, task)
				}
				if err != nil {
					markFailed(fmt.Sprintf("[添加STRM] [%s] 失败: %s (%v)", time.Now().Format("15:04"), task.LocalPath, err))
					slog.Error("[添加STRM] 失败", "进度", fmt.Sprintf("%d/%d", current, total), "path", filepath.Base(task.LocalPath), "error", err)
				} else {
					slog.Info("[添加STRM] 成功", "进度", fmt.Sprintf("%d/%d", current, total), "path", filepath.Base(task.LocalPath))
				}
			}
		})
	}
	cloudScan(ctx, startFid, strmPath, taskQueue)
	close(taskQueue)
	wg.Wait()

	if err := context.Cause(ctx); err != nil {
		slog.Warn("[任务中止] 生成strm过程中被中止", "error", err)
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

func cloudScan(ctx context.Context, fid string, currentPath string, taskQueue chan task) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}
	slog.Info("[添加strm] 获取云端列表", "path", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		slog.Error("[添加strm] 获取云端列表失败", "path", currentPath, "error", err)
		return
	}
	for _, item := range list {
		if err := context.Cause(ctx); err != nil {
			return
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

}

func doCreateStrm(ctx context.Context, t task) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, t.PC, t.FID)
	err := os.WriteFile(t.LocalPath, []byte(content), 0644)
	return err
}

func doDownloadFile(ctx context.Context, t task) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	_, url, _, err := open115.GetDownloadUrl(ctx, t.PC, "")
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status: %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(t.LocalPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(t.LocalPath)
	if err != nil {
		return err
	}

	defer func() {
		out.Close()
		if err != nil {
			os.Remove(t.LocalPath)
		}
	}()

	_, err = io.Copy(out, resp.Body)
	return err
}

func performMoveFiles(ctx context.Context) {
	if err := context.Cause(ctx); err != nil {
		return
	}
	targetCID, _, _, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		slog.Error("[添加strm] 无法获取移动文件", "error", err)
		return
	}
	fidsStr := strings.Join(rootFids, ",")
	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		markFailed(fmt.Sprintf("[添加STRM] [%s] 移动云盘文件失败: %v", time.Now().Format("15:04"), err))
		slog.Error("[添加strm] 移动云盘文件失败", "error", err)
	} else {
		slog.Info("[添加strm] 成功移动文件至 TempPath", "count", len(rootFids))
	}
}
