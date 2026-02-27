package syncFile

import (
	"115tools/config"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
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

func StartSync(parentCtx context.Context, mainWg *sync.WaitGroup) {
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
				runSync(parentCtx)
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
	slog.Info("[同步] 开始同步文件...")
	var ctx context.Context
	ctx, cancelFunc = context.WithCancelCause(parentCtx)
	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		slog.Info("[任务中止] 同步文件", "error", context.Cause(ctx))
		return
	}
	config := config.Get()
	rootPath := config.SyncPath
	if rootPath == "" {
		slog.Error("[同步] SyncPath 未设置")
		return
	}
	strmUrl = config.StrmUrl
	initDB()

	defer func() {
		cancelFunc(fmt.Errorf("[同步] 任务结束"))
		closeDB()
		slog.Info("[同步] 任务结束", "处理文件", stats.completed.Load(), "失败任务", stats.failed.Load())
	}()
	rootID, err := initRoot(ctx, config.SyncPath)
	if err != nil {
		slog.Error("[同步] 初始化失败", "error", err)
		return
	}

	if err := initTemp(ctx, config.TempPath); err != nil {
		slog.Error("[同步] Temp目录初始化失败", "error", err)
		return
	}

	if err := doSync(ctx, config.SyncPath, rootID); err != nil {
		slog.Error("[同步] 执行同步失败", "error", err)
		return
	}
	if stats.total.Load() == 0 {
		slog.Info("[同步] 没有需要同步的文件")
		return
	}
	slog.Info("[同步] 云端数据一致性校验...")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		slog.Info("[任务中止] 云端数据一致性校验", "error", context.Cause(ctx))
		return
	}
	wg.Go(func() {
		cleanUp(ctx, config.SyncPath, rootID)
	})
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 云端数据一致性校验", "error", err)
	}
	slog.Info("[同步] 云端数据一致性校验完成")
}
func initRoot(ctx context.Context, rootPath string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 初始化同步", "error", err)
		return "", err
	}
	fid := dbGetFID(rootPath)
	if fid != "" {
		return fid, nil
	}
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("[同步] 初次运行，开始初始化云端数据库...")
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
			slog.Warn("[同步] 云端扫描被中止，正在清理数据库", "error", err)
			dbClearPath(rootPath)
		}
		return "", err
	}

	slog.Info("[同步] 云端数据库初始化完成")
	return fid, nil
}
func initTemp(ctx context.Context, tempPath string) error {
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] Temp目录初始化", "error", err)
		return err
	}
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
func doSync(ctx context.Context, rootPath, rootID string) error {
	taskQueue := make(chan task, 1000)
	for range 3 {
		wg.Go(func() {
			for t := range taskQueue {
				if context.Cause(ctx) != nil {
					continue
				}
				current := stats.completed.Add(1)
				total := stats.total.Load()
				var err error
				if t.isStrm {
					err = handleStrmTask(t)
				} else {
					err = handleFileTask(t)
				}
				if err != nil {
					markFailed(fmt.Sprintf("[同步] [%s] 处理文件失败: %s (%v)", time.Now().Format("15:04"), t.path, err))
					slog.Error("[同步] 处理文件失败", "进度", fmt.Sprintf("%d/%d", current, total), "path", t.path, "error", err)
				} else {
					slog.Info("[同步] 处理文件成功", "进度", fmt.Sprintf("%d/%d", current, total), "path", t.path)
				}
			}
		})
	}
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 扫描本地文件", "error", err)
		return err
	}
	if err := localScan(ctx, rootPath, rootID, taskQueue); err != nil {
		return err
	}
	close(taskQueue)
	wg.Wait()
	return nil
}
func cloudScan(ctx context.Context, currentPath string) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}

	fid := dbGetFID(currentPath)
	slog.Info("[同步] 获取云端列表", "path", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		cancelFunc(fmt.Errorf("获取云端列表[%s]失败: %v", currentPath, err))
		return
	}
	for _, item := range list {
		if err := context.Cause(ctx); err != nil {
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
func localScan(ctx context.Context, currentPath string, currentCID string, taskQueue chan task) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(currentPath)
	if err != nil {
		cancelFunc(fmt.Errorf("读取本地文件夹[%s]: %v", currentPath, err))
		return err
	}

	dbFileMap := dbMapPool.Get().(map[string]string)
	clear(dbFileMap)
	defer dbMapPool.Put(dbFileMap)
	dbListChildren(currentPath, dbFileMap)

	localFound := localMapPool.Get().(map[string]bool)
	clear(localFound)
	defer localMapPool.Put(localFound)

	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return err
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
				slog.Info("[同步] 创建云端文件夹", "path", fullPath)
				newFid, err := open115.AddFolder(ctx, currentCID, name)
				if err != nil {
					cancelFunc(fmt.Errorf("创建云端文件夹[%s]失败: %v", fullPath, err))
					return err
				}
				dbSaveRecord(fullPath, newFid, -1)
				dbFid = newFid
			}
			if err := localScan(ctx, fullPath, dbFid, taskQueue); err != nil {
				return err
			}
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
		if err := context.Cause(ctx); err != nil {
			slog.Info("[任务中止] 扫描本地文件", "error", err)
			return err
		}
		if !localFound[dbFileName] {
			fullPath := currentPath + "/" + dbFileName
			fid, size, _ := strings.Cut(dbVal, "|")
			isDir := size == "-1"
			isStrm := strings.EqualFold(filepath.Ext(dbFileName), ".strm")
			cleanCloud(ctx, fullPath, fid, isDir || isStrm)
		}
	}
	return nil
}
func handleFileTask(t task) error {
	size := t.size
	savePath := t.path
	ext := filepath.Ext(t.path)
	isVideo := checkVideo(ext, t.size)
	if isVideo {
		savePath = t.path[:len(t.path)-len(ext)] + ".strm"
	}
	indexFid := dbGetFID(savePath)
	if indexFid != "" {
		if err := cleanCloud(t.ctx, savePath, indexFid, isVideo); err != nil {
			return err
		}
	}
	fid, pickcode, err := open115.UploadFile(t.ctx, t.path, t.cid, "", "")
	if err != nil {
		return err
	}
	if isVideo {
		if err := saveStrmFile(pickcode, fid, savePath); err != nil {
			slog.Error("[同步] 写入strm文件失败", "path", savePath, "error", err)
			return err
		}
		if err := os.Remove(t.path); err != nil {
			slog.Error("[同步] 删除原文件失败", "path", t.path, "error", err)
			return err
		}
		size = time.Now().Unix()
	}
	dbSaveRecord(savePath, fid, size)
	return nil
}
func handleStrmTask(t task) error {
	if err := context.Cause(t.ctx); err != nil {
		return err
	}
	indexFid := dbGetFID(t.path)
	if indexFid != "" {
		if err := cleanCloud(t.ctx, t.path, indexFid, t.isStrm); err != nil {
			return err
		}
	}
	contentBytes, _ := os.ReadFile(t.path)
	pickcode, fid := extractPickcode(string(contentBytes))
	if pickcode == "" {
		return fmt.Errorf("[同步] STRM内容无pickcode")
	}
	if fid == "" {
		cloudFid, _, _, err := open115.GetDownloadUrl(t.ctx, pickcode, "")
		if err != nil {
			return fmt.Errorf("[同步] 获取strm内视频fid失败: %v", err)
		}
		fid = cloudFid
	}

	if err := open115.MoveFile(t.ctx, fid, t.cid); err != nil {
		return fmt.Errorf("[同步] 移动strm内视频失败: %v", err)
	}

	targetPureName := strings.TrimSuffix(t.name, ".strm")
	newName, err := open115.UpdataFile(t.ctx, fid, targetPureName)
	if err != nil {
		return fmt.Errorf("[同步] strm内视频改名失败: %v", err)
	}

	realExt := filepath.Ext(newName)
	if strings.TrimSuffix(newName, realExt) != targetPureName {
		_, err = open115.UpdataFile(t.ctx, fid, targetPureName+realExt)
		if err != nil {
			return fmt.Errorf("[同步] strm内视频二次改名失败: %v", err)
		}
	}
	if err := saveStrmFile(pickcode, fid, t.path); err != nil {
		return fmt.Errorf("[同步] strm文件写入失败: %v", err)
	}
	dbSaveRecord(t.path, fid, time.Now().Unix())
	return nil
}
func cleanUp(ctx context.Context, currentPath string, cloudFID string) {
	select {
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	case <-ctx.Done():
		return
	}
	_, count, folderCount, err := open115.FolderInfo(ctx, currentPath)
	if err != nil {
		slog.Error("[清理] 获取文件夹信息失败", "path", currentPath, "error", err)
		return
	}
	cloudTotal := count + folderCount
	dbTotal := dbGetTotalCount(currentPath)
	if cloudTotal == dbTotal {
		return
	}
	slog.Info("[清理] 数量不一致", "path", currentPath, "云端数量", cloudTotal, "数据库数量", dbTotal)
	items, err := open115.FileList(ctx, cloudFID)
	if err != nil {
		slog.Error("[清理] 获取文件列表失败", "path", currentPath, "error", err)
		return
	}

	for _, item := range items {
		if err := context.Cause(ctx); err != nil {
			return
		}
		if item.Aid != "1" {
			continue
		}
		expectedPath := currentPath + "/" + item.Fn
		if item.Isv == 1 {
			expectedPath = expectedPath[:len(expectedPath)-len(filepath.Ext(expectedPath))] + ".strm"
		}
		recordedFid := dbGetFID(expectedPath)
		if recordedFid != item.Fid {
			slog.Info("[清理] 清理冗余项", "path", expectedPath)
			cleanCloud(ctx, expectedPath, item.Fid, item.Fc == "0")
		} else if item.Fc == "0" {
			wg.Go(func() {
				cleanUp(ctx, expectedPath, item.Fid)
			})
		}
	}
}
func cleanCloud(ctx context.Context, fPath, fid string, safe bool) error {
	var err error
	if safe {
		err = open115.MoveFile(ctx, fid, tempFid)
	} else {
		err = open115.DeleteFile(ctx, fid)
	}
	if err != nil && strings.Contains(err.Error(), "不存在或已经删除") {
		slog.Warn("[同步] 清理云端文件失败", "path", fPath, "error", err)
		err = nil
	}
	if err != nil {
		slog.Error("[同步] 清理云端文件失败", "path", fPath, "error", err)
		return err
	}
	dbClearPath(fPath)
	return nil
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
func saveStrmFile(pickcode, fid, localPath string) error {
	strmContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", strmUrl, pickcode, fid)
	if err := os.WriteFile(localPath, []byte(strmContent), 0644); err != nil {
		return err
	}
	return nil
}
