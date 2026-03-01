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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	tempFid      string
	needRetry    atomic.Bool
	wg           sync.WaitGroup
	sem          = make(chan struct{}, 3)
	isCloudScan  atomic.Bool
	cancelFunc   context.CancelCauseFunc
	localMapPool = sync.Pool{
		New: func() any { return make(map[string]struct{}, 256) },
	}
	dbMapPool = sync.Pool{
		New: func() any { return make(map[string]struct{}, 256) },
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

func StartSync(parentCtx context.Context) {
	needRetry.Store(true)
	if stats.running.CompareAndSwap(false, true) {
		defer stats.running.Store(false)
		stats.Reset()
		for needRetry.Swap(false) {
			if err := context.Cause(parentCtx); err != nil {
				slog.Error("[同步] 任务中止", "错误信息", err)
				return
			}
			runSync(parentCtx)
		}
	}
}

func StopSync() {
	if cancelFunc != nil {
		needRetry.Store(false)
		cancelFunc(fmt.Errorf("[同步] 用户请求停止同步"))
	}
}

func runSync(parentCtx context.Context) {
	slog.Info("[同步] 开始同步文件...")
	var ctx context.Context
	ctx, cancelFunc = context.WithCancelCause(parentCtx)
	select {
	case <-time.After(1 * time.Second):
	case <-ctx.Done():
		slog.Info("[任务中止] 同步文件", "错误信息", context.Cause(ctx))
		return
	}
	config := config.Get()
	rootPath := config.SyncPath
	if rootPath == "" {
		slog.Error("[同步] SyncPath 未设置")
		return
	}
	initDB()

	defer func() {
		cancelFunc(fmt.Errorf("[同步] 任务结束"))
		closeDB()
		slog.Info("[同步] 任务结束", "处理文件", stats.completed.Load(), "失败任务", stats.failed.Load())
	}()
	rootID, err := initRoot(ctx, config.SyncPath)
	if err != nil {
		slog.Error("[同步] 初始化失败", "错误信息", err)
		return
	}

	if err := initTemp(ctx, config.TempPath); err != nil {
		slog.Error("[同步] Temp目录初始化失败", "错误信息", err)
		return
	}

	if err := localSync(ctx, config.SyncPath, rootID); err != nil {
		slog.Error("[同步] 本地文件同步失败", "错误信息", err)
		return
	}
	if stats.failed.Load() > 0 {
		slog.Error("[同步] 本地文件同步失败,跳过云端一致性检验")
		return
	}
	slog.Info("[同步] 云端数据一致性校验...")
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		slog.Info("[任务中止] 云端数据一致性校验", "错误信息", context.Cause(ctx))
		return
	}
	wg.Go(func() {
		cloudSync(ctx, config.SyncPath, rootID)
	})
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 云端数据一致性校验", "错误信息", err)
	}
	slog.Info("[同步] 云端数据一致性校验完成")
}
func initRoot(ctx context.Context, rootPath string) (string, error) {
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 初始化同步", "错误信息", err)
		return "", err
	}
	fid := dbGetFid(rootPath)
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
			slog.Warn("[同步] 云端扫描被中止，正在清理数据库", "错误信息", err)
			dbClearPath(rootPath)
		}
		return "", err
	}

	slog.Info("[同步] 云端数据库初始化完成")
	return fid, nil
}
func initTemp(ctx context.Context, tempPath string) error {
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] Temp目录初始化", "错误信息", err)
		return err
	}
	tempFid = dbGetFid(tempPath)
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

type uploadTask struct {
	path   string
	cid    string
	name   string
	size   int64
	isStrm bool
}

func localScan(ctx context.Context, currentPath string, currentCID string, uploadTasks *[]uploadTask, deleteTasks *[]string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(currentPath)
	if err != nil {
		cancelFunc(fmt.Errorf("读取本地文件夹[%s]: %v", currentPath, err))
		return err
	}

	dbFileMap := dbMapPool.Get().(map[string]struct{})
	clear(dbFileMap)
	defer dbMapPool.Put(dbFileMap)
	dbListChildren(currentPath, dbFileMap)

	localFound := localMapPool.Get().(map[string]struct{})
	clear(localFound)
	defer localMapPool.Put(localFound)

	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		name := entry.Name()
		localFound[name] = struct{}{}
		fullPath := currentPath + "/" + name

		dbFid, dbSize := dbGetInfo(fullPath)

		if entry.IsDir() {
			if dbSize == -2 {
				slog.Info("[同步] 创建云端文件夹", "路径", fullPath)
				newFid, err := open115.AddFolder(ctx, currentCID, name)
				if err != nil {
					cancelFunc(fmt.Errorf("创建云端文件夹[%s]失败: %v", fullPath, err))
					return err
				}
				dbSaveRecord(fullPath, newFid, -1)
				dbFid = newFid
			}
			if err := localScan(ctx, fullPath, dbFid, uploadTasks, deleteTasks); err != nil {
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
				if dbSize != -2 {
					*deleteTasks = append(*deleteTasks, fullPath)
					stats.total.Add(1)
				}
				*uploadTasks = append(*uploadTasks, uploadTask{
					path: fullPath, cid: currentCID,
					name: name, size: size, isStrm: isStrm,
				})
				stats.total.Add(1)
			}
		}
	}

	for dbFileName := range dbFileMap {
		if err := context.Cause(ctx); err != nil {
			slog.Info("[任务中止] 扫描本地文件", "错误信息", err)
			return err
		}
		if _, exists := localFound[dbFileName]; !exists {
			fullPath := currentPath + "/" + dbFileName
			*deleteTasks = append(*deleteTasks, fullPath)
			stats.total.Add(1)
		}
	}
	return nil
}

func localSync(ctx context.Context, rootPath, rootID string) error {
	var uploadTasks []uploadTask
	var deleteTasks []string
	if err := localScan(ctx, rootPath, rootID, &uploadTasks, &deleteTasks); err != nil {
		return err
	}
	if len(uploadTasks) == 0 && len(deleteTasks) == 0 {
		return nil
	}

	delQueue := make(chan string, len(deleteTasks))
	for _, t := range deleteTasks {
		delQueue <- t
	}
	close(delQueue)
	for range 3 {
		wg.Go(func() {
			for fPath := range delQueue {
				if context.Cause(ctx) != nil {
					continue
				}
				if err := cloudCleanTask(ctx, fPath); err != nil {
					markFailed(fmt.Sprintf("[同步] [%s] 清理云端文件失败: %s (%v)", time.Now().Format("15:04"), fPath, err))
					slog.Error("[同步] 清理云端文件失败", "路径", fPath, "错误信息", err)
				}
			}
		})
	}
	wg.Wait()

	upQueue := make(chan uploadTask, len(uploadTasks))
	for _, t := range uploadTasks {
		upQueue <- t
	}
	close(upQueue)
	for range 3 {
		wg.Go(func() {
			for t := range upQueue {
				if context.Cause(ctx) != nil {
					continue
				}
				var err error
				if t.isStrm {
					err = upStrmTask(ctx, t)
				} else {
					err = upFileTask(ctx, t)
				}
				if err != nil {
					markFailed(fmt.Sprintf("[同步] [%s] 上传文件失败: %s (%v)", time.Now().Format("15:04"), t.path, err))
					slog.Error("[同步] 上传文件失败", "路径", t.path, "错误信息", err)
				} else {
					slog.Info("[同步] 上传文件成功", "路径", t.path)
				}
			}
		})
	}
	if err := context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 同步本地文件", "错误信息", err)
		return err
	}
	wg.Wait()
	return nil
}
func upFileTask(ctx context.Context, t uploadTask) error {
	fid, pickcode, err := open115.UploadFile(ctx, t.path, t.cid, "", "")
	if err != nil {
		return err
	}
	size := t.size
	savePath := t.path
	ext := filepath.Ext(t.path)
	isVideo := checkVideo(ext, t.size)
	if isVideo {
		savePath = t.path[:len(t.path)-len(ext)] + ".strm"
		indexFid := dbGetFid(savePath)
		if indexFid != "" {
			stats.total.Add(1)
			if err := cloudCleanTask(ctx, savePath); err != nil {
				return err
			}
		}
		if err := open115.SaveStrmFile(pickcode, fid, savePath); err != nil {
			slog.Error("[同步] 写入strm文件失败", "路径", savePath, "错误信息", err)
			return err
		}
		if err := os.Remove(t.path); err != nil {
			slog.Error("[同步] 删除原文件失败", "路径", t.path, "错误信息", err)
			return err
		}
		size = time.Now().Unix()
	}
	dbSaveRecord(savePath, fid, size)
	stats.completed.Add(1)
	return nil
}
func upStrmTask(ctx context.Context, t uploadTask) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	contentBytes, _ := os.ReadFile(t.path)
	pickcode, fid := extractPickcode(string(contentBytes))
	if pickcode == "" {
		return fmt.Errorf("[同步] STRM内容无pickcode")
	}
	if fid == "" {
		cloudFid, _, _, err := open115.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return fmt.Errorf("[同步] 获取strm内视频fid失败: %v", err)
		}
		fid = cloudFid
	}
	if fid == dbGetFid(t.path) {
		dbSaveRecord(t.path, fid, t.size)
		return nil
	}

	if err := open115.MoveFile(ctx, fid, t.cid); err != nil {
		return fmt.Errorf("[同步] 移动strm内视频失败: %v", err)
	}

	targetPureName := strings.TrimSuffix(t.name, ".strm")
	newName, err := open115.UpdataFile(ctx, fid, targetPureName)
	if err != nil {
		return fmt.Errorf("[同步] strm内视频改名失败: %v", err)
	}

	realExt := filepath.Ext(newName)
	if strings.TrimSuffix(newName, realExt) != targetPureName {
		_, err = open115.UpdataFile(ctx, fid, targetPureName+realExt)
		if err != nil {
			return fmt.Errorf("[同步] strm内视频二次改名失败: %v", err)
		}
	}
	if err := open115.SaveStrmFile(pickcode, fid, t.path); err != nil {
		return fmt.Errorf("[同步] strm文件写入失败: %v", err)
	}
	dbSaveRecord(t.path, fid, time.Now().Unix())
	stats.completed.Add(1)
	return nil
}
func cloudScan(ctx context.Context, currentPath string) {
	select {
	case <-ctx.Done():
		return
	case sem <- struct{}{}:
		defer func() { <-sem }()
	}

	fid := dbGetFid(currentPath)
	slog.Info("[同步] 获取云端列表", "路径", currentPath)
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
func cloudSync(ctx context.Context, currentPath string, cloudFID string) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}
	_, count, folderCount, err := open115.FolderInfo(ctx, currentPath)
	if err != nil {
		slog.Error("[同步] 获取文件夹信息失败", "路径", currentPath, "错误信息", err)
		return
	}
	cloudTotal := count + folderCount
	dbTotal := dbGetTotalCount(currentPath)
	if cloudTotal == dbTotal {
		return
	}
	slog.Info("[同步] 数量不一致", "路径", currentPath, "云端数量", cloudTotal, "数据库数量", dbTotal)
	items, err := open115.FileList(ctx, cloudFID)
	if err != nil {
		slog.Error("[同步] 获取文件列表失败", "路径", currentPath, "错误信息", err)
		return
	}

	for _, item := range items {
		if err := context.Cause(ctx); err != nil {
			return
		}
		if item.Aid != "1" {
			continue
		}
		cloudPath := currentPath + "/" + item.Fn
		if item.Fc == "0" {
			wg.Go(func() {
				cloudSync(ctx, cloudPath, item.Fid)
			})
			continue
		}
		var savedPath string
		isStrm := item.Isv == 1
		if isStrm {
			savedPath = cloudPath[:len(cloudPath)-len(filepath.Ext(cloudPath))] + ".strm"
		} else {
			savedPath = cloudPath
		}
		dbFid := dbGetFid(savedPath)
		if dbFid == "" {
			stats.total.Add(1)
			if isStrm {
				open115.SaveStrmFile(item.Pc, item.Fid, savedPath)
				slog.Info("[同步] 新增STRM文件", "路径", savedPath)
			} else {
				if err := open115.DownloadFile(ctx, item.Pc, savedPath); err != nil {
					markFailed(fmt.Sprintf("[同步] [%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				} else {
					slog.Info("[同步] 下载文件成功", "路径", savedPath)
				}
			}
			dbSaveRecord(savedPath, item.Fid, item.Fs)
			stats.completed.Add(1)
			continue
		}
		if dbFid != item.Fid {
			slog.Info("[同步] 清理云端冗余项", "路径", savedPath)
			stats.total.Add(1)
			if err := cloudCleanTask(ctx, savedPath); err != nil {
				markFailed(fmt.Sprintf("[同步] [%s] 清理云端冗余项失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
			}
		}
	}
}
func cloudCleanTask(ctx context.Context, fPath string) error {
	var err error
	fid, size := dbGetInfo(fPath)
	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
	if isStrm || size == -1 {
		err = open115.MoveFile(ctx, fid, tempFid)
		slog.Info("[同步] 移动云端文件到临时目录", "路径", fPath)
	} else {
		err = open115.DeleteFile(ctx, fid)
		slog.Info("[同步] 删除云端文件", "路径", fPath)
	}
	if err != nil && strings.Contains(err.Error(), "不存在或已经删除") {
		slog.Warn("[同步] 清理云端文件失败", "路径", fPath, "错误信息", err)
		err = nil
	}
	if err != nil {
		slog.Error("[同步] 清理云端文件失败", "路径", fPath, "错误信息", err)
		return err
	}
	dbClearPath(fPath)
	stats.completed.Add(1)
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
