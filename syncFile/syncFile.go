package syncFile

import (
	"115tools/open115"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	strmUrl      string
	tempFid      string
	needRetry    atomic.Bool
	wg           sync.WaitGroup
	scanSem      = make(chan struct{}, 3)
	isCloudScan  atomic.Bool
	cancelFunc   context.CancelCauseFunc
	localMapPool = sync.Pool{
		New: func() any { return make(map[string]bool, 256) },
	}
	dbMapPool = sync.Pool{
		New: func() any { return make(map[string]string, 256) },
	}
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
		Errors:    slices.Clone(stats.failedErrors),
		Running:   stats.running.Load(),
	}
}
func markFailed(reason string) {
	stats.failed.Add(1)
	stats.mu.Lock()
	stats.failedErrors = append(stats.failedErrors, reason)
	stats.mu.Unlock()
}

func StartSync(parentCtx context.Context, mainWg *sync.WaitGroup) {
	needRetry.Store(true)
	stats.Reset()
	if stats.running.CompareAndSwap(false, true) {
		mainWg.Go(func() {
			defer stats.running.Store(false)
			for needRetry.Swap(false) {
				if parentCtx.Err() != nil {
					return
				}
				runSync(parentCtx)
			}

			if needRetry.Load() {
				StartSync(parentCtx, mainWg)
			}
		})
	}
}

func StopSync() {
	if cancelFunc != nil {
		needRetry.Store(false)
		cancelFunc(fmt.Errorf("[同步] 用户请求停止同步"))
	}
}

type task struct {
	ctx    context.Context
	path   string
	cid    string
	name   string
	size   int64
	isStrm bool
}

func runSync(parentCtx context.Context) {
	var ctx context.Context
	ctx, cancelFunc = context.WithCancelCause(parentCtx)
	config := open115.Conf.Load()
	rootPath := config.SyncPath
	strmUrl = config.StrmUrl
	initDB()

	defer func() {
		cancelFunc(fmt.Errorf("[同步] 任务已彻底停止"))
		closeDB()
		log.Printf("[同步] 任务彻底停止")
	}()

	if rootPath == "" {
		log.Printf("[同步] SyncPath 未设置")
		return
	}

	log.Printf("[同步] 开始同步流程...")
	rootID, err := initRoot(ctx, config.SyncPath)
	if err != nil {
		log.Printf("[同步] 初始化失败: %v", err)
		return
	}
	if err := context.Cause(ctx); err != nil {
		log.Printf("[任务中止] 初始化同步 中止原因: %v", err)
		return
	}

	if err := initTemp(ctx, config.TempPath); err != nil {
		log.Printf("[同步] Temp目录准备失败: %v", err)
		return
	}
	if err := context.Cause(ctx); err != nil {
		log.Printf("[任务中止] Temp目录准备 中止原因: %v", err)
		return
	}

	doSync(ctx, config.SyncPath, rootID)
	if err := context.Cause(ctx); err != nil {
		log.Printf("[任务中止] 同步过程中被中止 中止原因: %v", err)
		return
	}

	time.Sleep(5 * time.Second)

	if err := context.Cause(ctx); err != nil {
		log.Printf("[任务中止] 云端数据一致性校验 中止原因: : %v", err)
		return
	}
	log.Printf("[同步] 云端数据一致性校验...")
	wg.Go(func() {
		cleanUp(ctx, config.SyncPath, rootID)
	})
	wg.Wait()
	log.Printf("[同步] 云端数据一致性校验完成")
}
func initRoot(ctx context.Context, rootPath string) (string, error) {
	fid := dbGetFID(rootPath)
	if fid != "" {
		return fid, nil
	}
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	log.Printf("[同步] 初次运行，开始初始化云端数据库...")
	fid, _, _, err := open115.FolderInfo(ctx, rootPath)
	if err != nil {
		return "", err
	}
	dbSaveRecord(rootPath, fid, -1)

	wg.Go(func() {
		cloudScan(ctx, rootPath)
	})
	wg.Wait()

	if err := context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			log.Printf("[同步] 云端扫描被中止，正在清理数据库 中止原因: %v", err)
			removeDB()
		}
		return "", err
	}

	log.Printf("[同步] 云端数据库初始化完成")
	return fid, nil
}
func initTemp(ctx context.Context, tempPath string) error {
	tempFid = dbGetFID(tempPath)
	if tempFid != "" {
		return nil
	}
	fid, _, _, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		return err
	}
	dbSaveRecord(tempPath, fid, -1)
	tempFid = fid
	return nil
}
func doSync(ctx context.Context, rootPath, rootID string) {
	log.Printf("[同步] 开始扫描本地文件并推送任务")
	taskQueue := make(chan task, 1000)
	for range 3 {
		wg.Go(func() {
			for t := range taskQueue {
				if ctx.Err() != nil {
					continue
				}
				current := stats.completed.Add(1)
				total := stats.total.Load()
				if err := processFile(t.ctx, t.path, t.cid, t.name, t.size, t.isStrm); err != nil {
					markFailed(fmt.Sprintf("[同步] [%s] 处理文件失败: %s (%v)", time.Now().Format("15:04"), t.path, err))
					log.Printf("[同步][%d/%d] ❌ %s: %v", current, total, t.path, err)
				} else {
					log.Printf("[同步][%d/%d] ✅ %s", current, total, t.path)
				}
			}
		})
	}
	localScan(ctx, rootPath, rootID, taskQueue)
	close(taskQueue)
	wg.Wait()
	log.Printf("[同步] 同步任务执行完成")
}
func cleanUp(ctx context.Context, currentPath string, cloudFID string) {
	select {
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	case <-ctx.Done():
		err := context.Cause(ctx)
		log.Printf("[任务中止] 云端文件夹校验: %s 中止原因: %v", currentPath, err)
		return
	}
	_, count, folderCount, err := open115.FolderInfo(ctx, currentPath)
	if err != nil {
		log.Printf("[清理] 获取文件夹信息失败: %s, %v", currentPath, err)
		return
	}
	cloudTotal := count + folderCount
	dbTotal := dbGetTotalCount(currentPath)
	if cloudTotal == dbTotal {
		return
	}

	log.Printf("[清理] 数量不一致: %s (云端:%d, 数据库:%d)，开始对比...", currentPath, cloudTotal, dbTotal)
	items, err := open115.FileList(ctx, cloudFID)
	if err != nil {
		return
	}

	for _, item := range items {
		if item.Aid != "1" {
			continue
		}
		expectedPath := currentPath + "/" + item.Fn
		if item.Isv == 1 {
			expectedPath = expectedPath[:len(expectedPath)-len(filepath.Ext(expectedPath))] + ".strm"
		}
		recordedFid := dbGetFID(expectedPath)
		if recordedFid != item.Fid {
			log.Printf("[清理] 发现冗余项: %s (FID: %s)", expectedPath, item.Fid)
			if item.Fc == "0" {
				open115.MoveFile(ctx, item.Fid, tempFid)
			} else {
				open115.DeleteFile(ctx, item.Fid)
			}
		} else if item.Fc == "0" {
			wg.Go(func() {
				cleanUp(ctx, expectedPath, item.Fid)
			})
		}
	}
}

func cloudScan(ctx context.Context, currentPath string) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}

	fid := dbGetFID(currentPath)
	log.Printf("[同步] 获取云端列表:%s", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		cancelFunc(fmt.Errorf("获取云端列表[%s]: %v", currentPath, err))
		return
	}
	for _, item := range list {
		if err := ctx.Err(); err != nil {
			return
		}
		if item.Aid != "1" {
			continue
		}
		savePath := currentPath + "/" + item.Fn
		saveSize := item.Fs
		if item.Fc == "0" {
			dbSaveRecord(savePath, item.Fid, -1)
			wg.Go(func() {
				cloudScan(ctx, savePath)
			})
		} else {
			if item.Isv == 1 {
				savePath = savePath[:len(savePath)-len(filepath.Ext(savePath))] + ".strm"
				saveSize = 0
				if info, err := os.Stat(savePath); err == nil {
					if content, err := os.ReadFile(savePath); err == nil {
						_, localFid := extractPickcode(string(content))
						if localFid == item.Fid {
							saveSize = info.ModTime().Unix()
						}
					}
				}
			}
			dbSaveRecord(savePath, item.Fid, saveSize)
		}
	}
}

func localScan(ctx context.Context, currentPath string, currentCID string, taskQueue chan task) {
	if err := ctx.Err(); err != nil {
		return
	}
	entries, err := os.ReadDir(currentPath)
	if err != nil {
		cancelFunc(fmt.Errorf("读取本地文件夹[%s]: %v", currentPath, err))
		return
	}

	dbFileMap := dbMapPool.Get().(map[string]string)
	clear(dbFileMap)
	defer dbMapPool.Put(dbFileMap)
	dbListChildren(currentPath, dbFileMap)

	localFound := localMapPool.Get().(map[string]bool)
	clear(localFound)
	defer localMapPool.Put(localFound)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return
		}
		name := entry.Name()
		localFound[name] = true
		fullPath := currentPath + "/" + name

		var dbFid string
		var dbSize int64 = -2
		if s, exists := dbFileMap[name]; exists {
			if before, after, ok := strings.Cut(s, "|"); ok {
				dbFid = before
				dbSize, _ = strconv.ParseInt(after, 10, 64)
			}
		}

		if entry.IsDir() {
			if dbSize == -2 {
				log.Printf("[同步] 创建云端文件夹: %s", fullPath)
				newFid, err := open115.AddFolder(ctx, currentCID, name)
				if err != nil {
					cancelFunc(fmt.Errorf("创建云端文件夹[%s]: %v", fullPath, err))
					return
				}
				dbSaveRecord(fullPath, newFid, -1)
				dbFid = newFid
			}
			localScan(ctx, fullPath, dbFid, taskQueue)
		} else {
			info, _ := entry.Info()
			isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
			size := info.Size()

			if isStrm {
				size = info.ModTime().Unix()
			}

			if dbSize == -2 || size != dbSize {
				taskQueue <- task{
					ctx: ctx, path: fullPath, cid: currentCID,
					name: name, size: size, isStrm: isStrm,
				}
				stats.total.Add(1)
			}
		}
	}

	for dbFileName, dbVal := range dbFileMap {
		if !localFound[dbFileName] {
			fullPath := currentPath + "/" + dbFileName
			fid, size, _ := strings.Cut(dbVal, "|")
			isDir := size == "-1"
			isStrm := strings.EqualFold(filepath.Ext(dbFileName), ".strm")
			var err error
			if isDir || isStrm {
				err = open115.MoveFile(ctx, fid, tempFid)
			} else {
				err = open115.DeleteFile(ctx, fid)
			}
			if err != nil && !strings.Contains(err.Error(), "不存在或已经删除") {
				log.Printf("[同步] 删除云端文件失败: %v", err)
				return
			}
			dbClearPath(fullPath)
			log.Printf("[同步] 删除云端文件: %s", fullPath)
		}
	}
}
func processFile(ctx context.Context, fPath, cid, name string, size int64, isStrm bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var err error
	indexPath := fPath
	ext := filepath.Ext(fPath)
	isVideo := checkVideo(ext, size)
	if isVideo {
		indexPath = fPath[:len(fPath)-len(ext)] + ".strm"
	}
	fid := dbGetFID(indexPath)
	if fid != "" {
		if isStrm {
			err = open115.MoveFile(ctx, fid, tempFid)
		} else {
			err = open115.DeleteFile(ctx, fid)
		}
		if err != nil && !strings.Contains(err.Error(), "不存在或已经删除") {
			log.Printf("[同步] 删除云端文件失败: %v", err)
			return err
		}
		err = nil
		dbClearPath(indexPath)
	}
	if isStrm {
		contentBytes, _ := os.ReadFile(fPath)
		fid, err = handleStrmTask(ctx, fPath, cid, name, string(contentBytes))
		if err == nil {
			dbSaveRecord(fPath, fid, time.Now().Unix())
		}
	} else {
		if err := ctx.Err(); err != nil {
			return err
		}
		var pickcode string
		fid, pickcode, err = open115.UploadFile(ctx, fPath, cid, "", "")
		if err == nil {
			if isVideo {
				baseUrl, _ := strings.CutSuffix(strmUrl, "/")
				strmContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", baseUrl, pickcode, fid)
				if err := os.WriteFile(indexPath, []byte(strmContent), 0644); err == nil {
					if err := os.Remove(fPath); err == nil {
						dbSaveRecord(indexPath, fid, time.Now().Unix())
					} else {
						log.Printf("[同步] 删除原文件失败: %v", err)
						return err
					}
				}
			} else {
				dbSaveRecord(fPath, fid, size)
			}
		}
	}
	return err
}

func handleStrmTask(ctx context.Context, fPath, cid, name, content string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var err error
	pickcode, fid := extractPickcode(content)
	if pickcode == "" {
		return "", fmt.Errorf("[同步] STRM内容无pickcode")
	}
	if fid == "" {
		fid, _, _, err = open115.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return "", fmt.Errorf("[同步] 获取strm内视频fid失败: %v", err)
		}
	}

	if err := open115.MoveFile(ctx, fid, cid); err != nil {
		return "", fmt.Errorf("[同步] 移动strm内视频失败: %v", err)
	}

	targetPureName := strings.TrimSuffix(name, ".strm")
	newName, err := open115.UpdataFile(ctx, fid, targetPureName)
	if err != nil {
		return "", fmt.Errorf("[同步] strm内视频改名失败: %v", err)
	}

	realExt := filepath.Ext(newName)
	if strings.TrimSuffix(newName, realExt) != targetPureName {
		_, err = open115.UpdataFile(ctx, fid, targetPureName+realExt)
		if err != nil {
			return "", fmt.Errorf("[同步] strm内视频二次改名失败: %v", err)
		}
	}

	newContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, pickcode, fid)
	if err := os.WriteFile(fPath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("[同步] strm文件写入内容失败: %v", err)
	}

	return fid, nil
}

func extractPickcode(content string) (string, string) {
	u, err := url.Parse(strings.TrimSpace(content))
	if err != nil {
		return "", ""
	}
	return u.Query().Get("pickcode"), u.Query().Get("fid")
}

func checkVideo(ext string, size int64) bool {
	if size < 10*1024*1024 {
		return false
	}
	switch strings.ToLower(ext) {
	case ".mp4", ".mkv", ".avi", ".mov", ".ts", ".flv", ".wmv":
		return true
	}
	return false
}
