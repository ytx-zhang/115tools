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

type task struct {
	Strm      bool
	PC        string
	FID       string
	LocalPath string
}

var (
	strmPath     string
	tempPath     string
	strmUrl      string
	rootFids     []string
	doneTasks    atomic.Int32
	failedTasks  atomic.Int32
	activeMsgMap sync.Map // 存储活跃任务描述
	recentErrors []string // 存储最近错误
	errorMu      sync.Mutex
	isRunning    atomic.Bool
	wg           sync.WaitGroup
	scanSem      = make(chan struct{}, 3)
	cancelFunc   context.CancelFunc
)

func updateTaskMsg(key string, msg string) {
	if msg == "" {
		activeMsgMap.Delete(key)
	} else {
		activeMsgMap.Store(key, msg)
	}
}

// AddTaskError 记录错误
func addTaskError(errStr string) {
	errorMu.Lock()
	defer errorMu.Unlock()
	recentErrors = append([]string{errStr}, recentErrors...)
}

type StrmStatus struct {
	Running bool     `json:"running"`
	Done    int32    `json:"done"`
	Failed  int32    `json:"failed"`
	Active  []string `json:"active"` // 正在扫描的目录或正在处理的文件
	Errors  []string `json:"errors"` // 最近生成的错误信息
}

func GetStatus() StrmStatus {
	var msgs []string
	activeMsgMap.Range(func(key, value any) bool {
		msgs = append(msgs, value.(string))
		return true
	})

	errorMu.Lock()
	errs := make([]string, len(recentErrors))
	copy(errs, recentErrors)
	errorMu.Unlock()

	return StrmStatus{
		Running: isRunning.Load(),
		Done:    doneTasks.Load(),
		Failed:  failedTasks.Load(),
		Active:  msgs,
		Errors:  errs,
	}
}

func StartAddStrm(parentCtx context.Context, mainWg *sync.WaitGroup) {
	if isRunning.Load() {
		log.Printf("[添加strm] 检测到正在运行，准备下发停止信号...")
		cancelFunc()
		return
	}

	mainWg.Go(func() {
		var ctx context.Context
		ctx, cancelFunc = context.WithCancel(parentCtx)
		isRunning.Store(true)
		doneTasks.Store(0)
		failedTasks.Store(0)
		errorMu.Lock()
		recentErrors = nil // 清空错误列表
		errorMu.Unlock()
		activeMsgMap.Range(func(k, v any) bool {
			activeMsgMap.Delete(k) // 清空残留的活跃消息
			return true
		})
		conf := open115.Conf.Load()
		strmPath = conf.StrmPath
		strmUrl = conf.StrmUrl
		tempPath = conf.TempPath

		defer func() {
			cancelFunc()
			isRunning.Store(false)
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
		var allFileTasks []task // 本次任务的局部切片，预分配容量以优化性能
		var mu sync.Mutex
		rootFids = nil
		doneTasks.Store(0)
		wg.Go(func() {
			scanCloudRecursive(ctx, startCID, strmPath, &allFileTasks, &mu)
		})
		wg.Wait()

		// 扫描结束后检查是否已被取消
		if ctx.Err() != nil {
			log.Printf("[添加strm] 扫描阶段被中止")
			return
		}

		total := int32(len(allFileTasks))
		if total == 0 {
			log.Printf("[添加strm] 未发现新任务")
			return
		}

		// 执行阶段
		log.Printf("[添加strm] 开始执行任务，总计: %d", total)
		taskQueue := make(chan task, 3)
		const workerCount = 3
		for range workerCount {
			wg.Go(func() {
				for task := range taskQueue {
					current := doneTasks.Add(1)
					if task.Strm {
						doCreateStrm(ctx, task, int32(current), total)
					} else {
						doDownloadFile(ctx, task, int32(current), total)
					}
				}
			})
		}
		go func() {
			for _, t := range allFileTasks {
				taskQueue <- t
			}
			close(taskQueue)
		}()
		// 等待所有协程结束
		wg.Wait()

		// 移动文件逻辑：仅在任务未被取消且全部成功时执行
		if ctx.Err() == nil && len(rootFids) > 0 {
			performMoveFiles(ctx)
		}
	})
}

// scanCloudRecursive 递归扫描云端，支持 Context 取消
func scanCloudRecursive(ctx context.Context, cid string, localPath string, allFileTasks *[]task, mu *sync.Mutex) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}
	taskKey := "addstrm" + localPath
	updateTaskMsg(taskKey, fmt.Sprintf("获取云端列表: %s", localPath))
	defer updateTaskMsg(taskKey, "")

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
			wg.Go(func() { scanCloudRecursive(ctx, item.Fid, currentLocalPath, allFileTasks, mu) })
		} else { // 文件逻辑
			finalPath := currentLocalPath
			if item.Isv == 1 {
				finalPath = finalPath[:len(finalPath)-len(filepath.Ext(finalPath))] + ".strm"
			}

			// 存在则跳过
			if _, err := os.Stat(finalPath); err == nil {
				continue
			}
			t := task{
				Strm:      item.Isv == 1,
				PC:        item.Pc,
				FID:       item.Fid,
				LocalPath: finalPath,
			}
			mu.Lock()
			*allFileTasks = append(*allFileTasks, t)
			mu.Unlock()
		}
	}

}

// doCreateStrm 生成 STRM 文件
func doCreateStrm(ctx context.Context, t task, current, total int32) {
	if ctx.Err() != nil {
		return
	}
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, t.PC, t.FID)
	err := os.WriteFile(t.LocalPath, []byte(content), 0644)
	if err == nil {
		log.Printf("[%d/%d][添加STRM] 生成strm成功: %s", current, total, filepath.Base(t.LocalPath))
	} else {
		failedTasks.Add(1)
		addTaskError(fmt.Sprintf("[添加STRM] [%s] 生成strm失败: %s (%v)", time.Now().Format("15:04"), t.LocalPath, err))
		log.Printf("[%d/%d][添加STRM] 生成strm失败: %s: %v", current, total, filepath.Base(t.LocalPath), err)
	}
}

// doDownloadFile 处理文件下载，支持网络中断
func doDownloadFile(ctx context.Context, t task, current, total int32) {
	if ctx.Err() != nil {
		return
	}
	taskKey := "addstrm" + t.LocalPath
	updateTaskMsg(taskKey, fmt.Sprintf("下载文件: %s", t.LocalPath))
	defer updateTaskMsg(taskKey, "")
	// 使用辅助函数执行下载，统一处理报错
	if err := downloadToFile(ctx, t); err != nil {
		failedTasks.Add(1)
		addTaskError(fmt.Sprintf("[添加STRM] [%s] 下载失败: %s (%v)", time.Now().Format("15:04"), t.LocalPath, err))
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
	taskKey := "addstrm_move"
	updateTaskMsg(taskKey, fmt.Sprintf("移动云盘文件至TempPath: %s", tempPath))
	defer updateTaskMsg(taskKey, "")

	targetCID, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		log.Printf("[添加strm] 无法获取移动文件: %v", err)
		return
	}
	fidsStr := strings.Join(rootFids, ",")
	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		addTaskError(fmt.Sprintf("[添加STRM] [%s] 移动云盘文件失败: %v", time.Now().Format("15:04"), err))
		log.Printf("[添加strm] 移动云盘文件失败: %v", err)
	} else {
		log.Printf("[添加strm] 成功移动 %d 个文件至 TempPath", len(rootFids))
	}
}
