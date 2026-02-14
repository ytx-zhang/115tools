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
)

type task struct {
	Type      string // "STRM" or "DOWNLOAD"
	PC        string
	FID       string
	LocalPath string
}

var (
	strmPath    string
	tempPath    string
	strmUrl     string
	rootFids    []string
	doneTasks   atomic.Int32
	failedTasks atomic.Int32
	isRunning   atomic.Bool
	wg          sync.WaitGroup
	scanSem     = make(chan struct{}, 3)
	cancelFunc  context.CancelFunc
	logs        []string
	logsMutex   sync.Mutex
)

func GetStatus() (bool, int32, int32) {
	return isRunning.Load(), doneTasks.Load(), failedTasks.Load()
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
		logsMutex.Lock()
		logs = logs[:0]
		logsMutex.Unlock()
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
					if task.Type == "STRM" {
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
				ext := filepath.Ext(item.Fn)
				finalPath = strings.TrimSuffix(currentLocalPath, ext) + ".strm"
			}

			// 存在则跳过
			if _, err := os.Stat(finalPath); err == nil {
				continue
			}
			t := task{
				Type:      map[int]string{1: "STRM", 0: "DOWNLOAD"}[item.Isv],
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

	baseUrl, _ := strings.CutSuffix(strmUrl, "/")
	content := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", baseUrl, t.PC, t.FID)
	err := os.WriteFile(t.LocalPath, []byte(content), 0644)
	if err == nil {
		log.Printf("[%d/%d][添加STRM] 生成strm成功: %s", current, total, filepath.Base(t.LocalPath))
	} else {
		failedTasks.Add(1)
		log.Printf("[%d/%d][添加STRM] 生成strm失败: %s: %v", current, total, filepath.Base(t.LocalPath), err)
	}
}

// doDownloadFile 处理文件下载，支持网络中断
func doDownloadFile(ctx context.Context, t task, current, total int32) {
	if ctx.Err() != nil {
		return
	}
	// 使用辅助函数执行下载，统一处理报错
	if err := downloadToFile(ctx, t); err != nil {
		failedTasks.Add(1)
		log.Printf("[%d/%d][添加STRM] 失败: %s: %v", current, total, t.LocalPath, err)
		return
	}
	log.Printf("[%d/%d][添加STRM] 下载成功: %s", current, total, filepath.Base(t.LocalPath))
}

func downloadToFile(ctx context.Context, t task) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	downloadInfo, err := open115.GetDownloadUrl(ctx, t.PC, "")
	if err != nil {
		return err
	}
	url := downloadInfo.URL
	// 1. 发起请求：Context 会自动管理超时和取消
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

	// 2. 准备目录和文件
	// Go 1.22+ 优化：MkdirAll 的性能更高了
	if err := os.MkdirAll(filepath.Dir(t.LocalPath), 0755); err != nil {
		return err
	}

	out, err := os.Create(t.LocalPath)
	if err != nil {
		return err
	}
	defer out.Close()

	// 3. 核心简化：io.Copy 会响应 Body 的取消信号
	// 因为 resp.Body 是从携带 Context 的 Request 产生的，
	// 一旦 ctx 取消，Body.Read 会立即返回 context canceled
	_, err = io.Copy(out, resp.Body)
	return err
}

// 提取出的移动云盘文件逻辑
func performMoveFiles(ctx context.Context) {
	targetCID, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		log.Printf("[添加strm] 无法获取移动目标目录: %v", err)
		return
	}
	fidsStr := strings.Join(rootFids, ",")
	if err := open115.MoveFile(ctx, fidsStr, targetCID); err != nil {
		log.Printf("[添加strm] 移动云盘文件失败: %v", err)
	} else {
		log.Printf("[添加strm] 成功移动 %d 个文件至 TempPath", len(rootFids))
	}
}
