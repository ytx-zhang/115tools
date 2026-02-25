package addStrm

import (
	"115tools/open115"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
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

var stats = &taskStats{}

func (s *taskStats) Reset() {
	s.total.Store(0)
	s.completed.Store(0)
	s.failed.Store(0)
	s.mu.Lock()
	s.failedErrors = s.failedErrors[:0]
	s.mu.Unlock()
	s.running.Store(false)
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

	errors := append(make([]string, 0, len(stats.failedErrors)), stats.failedErrors...)
	return TaskStatsJSON{
		Total:     stats.total.Load(),
		Completed: stats.completed.Load(),
		Failed:    stats.failed.Load(),
		Errors:    errors,
		Running:   stats.running.Load(),
	}
}
func markFailed(reason string) {
	stats.failed.Add(1)
	stats.mu.Lock()
	stats.failedErrors = append(stats.failedErrors, reason)
	stats.mu.Unlock()
}

type task struct {
	Strm      bool
	PC        string
	FID       string
	LocalPath string
}

func StartAddStrm(parentCtx context.Context, mainWg *sync.WaitGroup) {
	if stats.running.Load() {
		log.Printf("[添加strm] 检测到正在运行的任务，无法启动新任务")
		return
	}

	mainWg.Go(func() {
		var ctx context.Context
		ctx, cancelFunc = context.WithCancelCause(parentCtx)
		stats.Reset()
		stats.running.Store(true)
		conf := open115.Conf.Load()
		strmPath = conf.StrmPath
		strmUrl = conf.StrmUrl
		tempPath = conf.TempPath

		defer func() {
			cancelFunc(fmt.Errorf("[添加strm] 任务彻底停止"))
			stats.running.Store(false)
			rootFids = nil
			log.Printf("[添加strm] 任务彻底停止")
		}()

		log.Printf("[添加strm] 正在获取起始目录id: %v", strmPath)
		startFid, _, _, err := open115.FolderInfo(ctx, strmPath)
		if err != nil {
			log.Printf("[添加strm] 无法获取起始目录id: %v", err)
			return
		}

		log.Printf("[添加strm] 正在扫描云端目录结构...")
		taskQueue := make(chan task, 1000)
		for range 3 {
			wg.Go(func() {
				for task := range taskQueue {
					if ctx.Err() != nil {
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
					if err == nil {
						log.Printf("[%d/%d][添加STRM] 成功: %s", current, total, filepath.Base(task.LocalPath))
					} else {
						markFailed(fmt.Sprintf("[添加STRM] [%s] 失败: %s (%v)", time.Now().Format("15:04"), task.LocalPath, err))
						log.Printf("[%d/%d][添加STRM] 失败: %s: %v", current, total, filepath.Base(task.LocalPath), err)
					}
				}
			})
		}
		cloudScan(ctx, startFid, strmPath, taskQueue)
		close(taskQueue)
		wg.Wait()

		if err := context.Cause(ctx); err != nil {
			log.Printf("[任务中止] 生成strm过程中被中止 取消原因: %v", err)
		}

		if len(stats.failedErrors) == 0 && len(rootFids) > 0 {
			performMoveFiles(ctx)
		}
	})
}
func StopAddStrm() {
	if cancelFunc != nil {
		cancelFunc(fmt.Errorf("[添加strm] 用户请求停止任务"))
	}
}

func cloudScan(ctx context.Context, fid string, currentPath string, taskQueue chan task) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}
	log.Printf("[添加strm] 正在扫描云端目录: %s", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		log.Printf("[添加strm] 读取目录失败 %s: %v", currentPath, err)
		return
	}
	for _, item := range list {
		if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return err
	}
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, t.PC, t.FID)
	err := os.WriteFile(t.LocalPath, []byte(content), 0644)
	return err
}

func doDownloadFile(ctx context.Context, t task) error {
	if err := ctx.Err(); err != nil {
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
	if err := ctx.Err(); err != nil {
		return
	}
	targetCID, _, _, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		log.Printf("[添加strm] 无法获取移动文件: %v", err)
		return
	}
	fidsStr := strings.Join(rootFids, ",")
	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		markFailed(fmt.Sprintf("[添加STRM] [%s] 移动云盘文件失败: %v", time.Now().Format("15:04"), err))
		log.Printf("[添加strm] 移动云盘文件失败: %v", err)
	} else {
		log.Printf("[添加strm] 成功移动 %d 个文件至 TempPath", len(rootFids))
	}
}
