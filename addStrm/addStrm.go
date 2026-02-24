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
	cancelFunc context.CancelFunc
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

	return TaskStatsJSON{
		Total:     stats.total.Load(),
		Completed: stats.completed.Load(),
		Failed:    stats.failed.Load(),
		Errors:    append([]string(nil), stats.failedErrors...),
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
		log.Printf("[添加strm] 检测到正在运行，准备下发停止信号...")
		cancelFunc()
		return
	}

	mainWg.Go(func() {
		var ctx context.Context
		ctx, cancelFunc = context.WithCancel(parentCtx)
		stats.Reset()
		stats.running.Store(true)
		conf := open115.Conf.Load()
		strmPath = conf.StrmPath
		strmUrl = conf.StrmUrl
		tempPath = conf.TempPath

		defer func() {
			cancelFunc()
			stats.running.Store(false)
			log.Printf("[添加strm] 任务彻底停止")
		}()

		log.Printf("[添加strm] 正在获取起始目录id: %v", strmPath)
		startCID, err := open115.FolderInfo(ctx, strmPath)
		if err != nil {
			log.Printf("[添加strm] 无法获取起始目录id: %v", err)
			return
		}

		// 扫描阶段：递归寻找需要处理的文件
		log.Printf("[添加strm] 正在扫描云端目录结构...")
		rootFids = nil
		taskQueue := make(chan task, 10000)
		for range 3 {
			wg.Go(func() {
				for task := range taskQueue {
					if ctx.Err() != nil {
						continue
					}
					current := stats.completed.Add(1)
					if task.Strm {
						doCreateStrm(ctx, task, current, stats.total.Load())
					} else {
						doDownloadFile(ctx, task, current, stats.total.Load())
					}
				}
			})
		}
		scanCloudRecursive(ctx, startCID, strmPath, taskQueue)
		close(taskQueue)
		wg.Wait()

		// 移动文件逻辑：仅在任务未被取消且全部成功时执行
		if ctx.Err() == nil && len(rootFids) > 0 {
			performMoveFiles(ctx)
		}
	})
}
func StopAddStrm() {
	if cancelFunc != nil {
		cancelFunc()
		log.Printf("[添加STRM] 正在中止当前运行中的任务...")
	}
}

// scanCloudRecursive 递归扫描云端，支持 Context 取消
func scanCloudRecursive(ctx context.Context, cid string, localPath string, taskQueue chan task) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}

	log.Printf("[添加strm] 正在扫描云端目录: %s", localPath)
	list, err := open115.FileList(ctx, cid)
	if err != nil {
		log.Printf("[添加strm] 读取目录失败 %s: %v", localPath, err)
		return
	}
	for _, item := range list {
		currentLocalPath := filepath.Join(localPath, item.Fn)
		if localPath == strmPath {
			rootFids = append(rootFids, item.Fid)
		}

		if item.Fc == "0" { // 文件夹
			_ = os.MkdirAll(currentLocalPath, 0755)
			scanCloudRecursive(ctx, item.Fid, currentLocalPath, taskQueue)
		} else { // 文件逻辑
			finalPath := currentLocalPath
			if item.Isv == 1 {
				finalPath = finalPath[:len(finalPath)-len(filepath.Ext(finalPath))] + ".strm"
			}

			// 存在则跳过
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

// doCreateStrm 生成 STRM 文件
func doCreateStrm(ctx context.Context, t task, current, total int64) {
	if ctx.Err() != nil {
		return
	}
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, t.PC, t.FID)
	err := os.WriteFile(t.LocalPath, []byte(content), 0644)
	if err == nil {
		log.Printf("[%d/%d][添加STRM] 生成strm成功: %s", current, total, filepath.Base(t.LocalPath))
	} else {
		stats.failed.Add(1)
		markFailed(fmt.Sprintf("[添加STRM] [%s] 生成strm失败: %s (%v)", time.Now().Format("15:04"), t.LocalPath, err))
		log.Printf("[%d/%d][添加STRM] 生成strm失败: %s: %v", current, total, filepath.Base(t.LocalPath), err)
	}
}

// doDownloadFile 处理文件下载，支持网络中断
func doDownloadFile(ctx context.Context, t task, current, total int64) {
	if ctx.Err() != nil {
		return
	}
	// 使用辅助函数执行下载，统一处理报错
	if err := downloadToFile(ctx, t); err != nil {
		stats.failed.Add(1)
		markFailed(fmt.Sprintf("[添加STRM] [%s] 下载失败: %s (%v)", time.Now().Format("15:04"), t.LocalPath, err))
		log.Printf("[%d/%d][添加STRM] 下载失败: %s: %v", current, total, t.LocalPath, err)
		return
	}
	log.Printf("[%d/%d][添加STRM] 下载成功: %s", current, total, t.LocalPath)
}

func downloadToFile(ctx context.Context, t task) error {
	if ctx.Err() != nil {
		return ctx.Err()
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
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

// 移动云盘文件逻辑
func performMoveFiles(ctx context.Context) {
	targetCID, err := open115.FolderInfo(ctx, tempPath)
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
