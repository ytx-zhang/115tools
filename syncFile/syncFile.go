package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/tidwall/sjson"
)

var (
	syncPath        string
	syncFid         string
	tempPath        string
	tempFid         string
	wg              sync.WaitGroup
	sem             = make(chan struct{}, 2)
	localCancelFunc context.CancelCauseFunc
	cloudCancelFunc context.CancelCauseFunc
	localMapPool    = sync.Pool{
		New: func() any { return make(map[string]struct{}, 1000) },
	}
	dbMapPool = sync.Pool{
		New: func() any { return make(map[string]struct{}, 1000) },
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

func GetStatus() string {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	data, _ := sjson.Set("", "total", stats.total.Load())
	data, _ = sjson.Set(data, "completed", stats.completed.Load())
	data, _ = sjson.Set(data, "failed", stats.failed.Load())
	data, _ = sjson.Set(data, "running", stats.running.Load())
	data, _ = sjson.Set(data, "errors", stats.failedErrors)
	return data
}
func markFailed(reason string) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.failed.Add(1)
	stats.failedErrors = append(stats.failedErrors, reason)
}

func StartCloudSync(parentCtx context.Context) {
	if stats.running.CompareAndSwap(false, true) {
		defer stats.running.Store(false)
		stats.Reset()
		var ctx context.Context
		ctx, cloudCancelFunc = context.WithCancelCause(parentCtx)
		slog.Info("[同步] 开始同步云端文件...")
		wg.Go(func() { cloudSync(ctx, syncPath, syncFid) })
		wg.Wait()
		slog.Info("[同步] 任务结束", "总数", stats.total.Load())
	}
}

func StopCloudSync() {
	if cloudCancelFunc != nil {
		cloudCancelFunc(fmt.Errorf("[同步] 用户请求停止同步"))
	}
}

func InitSync(parentCtx context.Context) (err error) {
	slog.Info("开始初始化...")
	var ctx context.Context
	ctx, localCancelFunc = context.WithCancelCause(parentCtx)

	config := config.Get()
	syncPath = config.SyncPath
	tempPath = config.TempPath
	db.Init()

	if syncPath == "" {
		return fmt.Errorf("SyncPath 未设置")
	}

	if syncFid, err = initRoot(ctx); err != nil {
		return
	}

	if tempFid, err = initTemp(ctx); err != nil {
		return
	}

	if err := localSync(ctx, syncPath, syncFid); err != nil {
		slog.Warn("本地文件同步失败", "错误信息", err)
	}
	wm := &WatchManager{}
	go wm.startWatch(ctx)
	slog.Info("初始化结束")
	return
}

type WatchManager struct {
	mu           sync.Mutex
	pendingTasks map[string]struct{}
	isRunning    bool
}

func (m *WatchManager) startWatch(ctx context.Context) {
	m.pendingTasks = make(map[string]struct{})
	watcher, err := fswatcher.New(
		fswatcher.WithPath(syncPath),
		fswatcher.WithEventBatching(2*time.Second),
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	slog.Info("文件监听器启动", "路径", syncPath)
	go watcher.Watch(ctx)

	for event := range watcher.Events() {
		dir := filepath.Dir(event.Path)

		m.mu.Lock()
		m.pendingTasks[dir] = struct{}{}
		if !m.isRunning {
			m.isRunning = true
			go m.runWatchTask(ctx)
		}
		m.mu.Unlock()
	}
}

func (m *WatchManager) runWatchTask(ctx context.Context) {
	defer func() {
		m.mu.Lock()
		m.isRunning = false
		m.mu.Unlock()
	}()

	for {
		m.mu.Lock()
		if len(m.pendingTasks) == 0 {
			m.mu.Unlock()
			return
		}
		todo := make([]string, 0, len(m.pendingTasks))
		for p := range m.pendingTasks {
			todo = append(todo, p)
			delete(m.pendingTasks, p)
		}
		m.mu.Unlock()
		for _, path := range todo {
			select {
			case <-ctx.Done():
				return
			default:
				fid := db.GetFid(path)
				if fid != "" {
					if _, err := os.Stat(path); err == nil {
						slog.Info("执行目录同步", "路径", path)
						localSync(ctx, path, fid)
					}
				}
			}
		}
	}
}
func initRoot(ctx context.Context) (fid string, err error) {
	if err = context.Cause(ctx); err != nil {
		slog.Info("[任务中止] 初始化同步", "错误信息", err)
		return
	}
	fid = db.GetFid(syncPath)
	if fid != "" {
		return
	}
	var isCloudScan atomic.Bool
	isCloudScan.Store(true)
	defer isCloudScan.Store(false)
	slog.Info("初次运行，开始初始化云端数据库...")
	if fid, _, _, err = open115.FolderInfo(ctx, syncPath); err != nil {
		return
	}
	db.SaveRecord(syncPath, fid, -1)

	wg.Go(func() {
		cloudScan(ctx, syncPath)
	})
	wg.Wait()

	if err = context.Cause(ctx); err != nil {
		if isCloudScan.Load() {
			slog.Error("云端扫描被中止，正在清理数据库", "错误信息", err)
			db.ClearPath(syncPath)
		}
		return
	}

	slog.Info("[同步] 云端数据库初始化完成")
	return
}
func initTemp(ctx context.Context) (fid string, err error) {
	if err = context.Cause(ctx); err != nil {
		slog.Info("[任务中止] Temp目录初始化", "错误信息", err)
		return
	}
	fid = db.GetFid(tempPath)
	if fid != "" {
		return
	}
	fid, _, _, err = open115.FolderInfo(ctx, tempPath)
	if err != nil {
		return
	}
	db.SaveRecord(tempPath, fid, -1)
	return
}

type uploadTask struct {
	path   string
	cid    string
	name   string
	size   int64
	isStrm bool
}

func localSync(ctx context.Context, workPath, workFid string) error {
	var uploadTasks []uploadTask
	var deleteTasks []string
	if err := localScan(ctx, workPath, workFid, &uploadTasks, &deleteTasks); err != nil {
		return err
	}
	if len(uploadTasks) == 0 && len(deleteTasks) == 0 {
		return nil
	}
	for _, fPath := range deleteTasks {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := cloudCleanTask(ctx, fPath); err != nil {
				slog.Error("清理失败", "路径", fPath, "错误信息", err)
			}
		})
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return err
	}
	for _, t := range uploadTasks {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			uploadFunc := upFileTask
			if t.isStrm {
				uploadFunc = upStrmTask
			}
			if err := uploadFunc(ctx, t); err != nil {
				slog.Error("上传失败", "路径", t.path, "错误信息", err)
			} else {
				slog.Info("上传成功", "路径", t.path)
			}
		})
	}
	wg.Wait()
	return nil
}
func localScan(ctx context.Context, workPath string, workFid string, uploadTasks *[]uploadTask, deleteTasks *[]string) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	entries, err := os.ReadDir(workPath)
	if err != nil {
		return err
	}

	dbFileMap := dbMapPool.Get().(map[string]struct{})
	clear(dbFileMap)
	defer dbMapPool.Put(dbFileMap)
	db.ListChildren(workPath, dbFileMap)

	localFound := localMapPool.Get().(map[string]struct{})
	clear(localFound)
	defer localMapPool.Put(localFound)

	for _, entry := range entries {
		if err := context.Cause(ctx); err != nil {
			return err
		}

		name := entry.Name()
		fullPath := workPath + "/" + name
		localFound[name] = struct{}{}

		dbFid, dbSize := db.GetInfo(fullPath)
		isNew := dbSize == -2

		if entry.IsDir() {
			if isNew {
				fid, err := addCloudFolder(ctx, workFid, name, fullPath)
				if err != nil {
					localCancelFunc(err)
					return err
				}
				db.SaveRecord(fullPath, fid, -1)
				dbFid = fid
			}
			if err := localScan(ctx, fullPath, dbFid, uploadTasks, deleteTasks); err != nil {
				return err
			}
			continue
		}

		info, _ := entry.Info()
		isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
		size := info.Size()
		if isStrm {
			size = info.ModTime().Unix()
		}

		isModified := (!isNew && size != dbSize)
		if !isNew && !isModified {
			continue
		}
		if isModified {
			*deleteTasks = append(*deleteTasks, fullPath)
		}
		*uploadTasks = append(*uploadTasks, uploadTask{
			path: fullPath, cid: workFid,
			name: name, size: size, isStrm: isStrm,
		})
	}

	for dbFileName := range dbFileMap {
		if err := context.Cause(ctx); err != nil {
			slog.Info("[任务中止] 扫描本地文件", "错误信息", err)
			return err
		}
		if _, exists := localFound[dbFileName]; !exists {
			fullPath := workPath + "/" + dbFileName
			*deleteTasks = append(*deleteTasks, fullPath)
		}
	}

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
		indexFid := db.GetFid(savePath)
		if indexFid != "" {
			stats.total.Add(1)
			if err := cloudCleanTask(ctx, savePath); err != nil {
				slog.Error("清理过时视频失败", "路径", savePath, "错误信息", err)
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
	db.SaveRecord(savePath, fid, size)
	return nil
}
func upStrmTask(ctx context.Context, t uploadTask) error {
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
	if fid == db.GetFid(t.path) {
		db.SaveRecord(t.path, fid, t.size)
		return nil
	}

	if err := open115.MoveFile(ctx, fid, t.cid); err != nil {
		return fmt.Errorf("[同步] 移动strm内视频失败: %v", err)
	}

	targetPureName := strings.TrimSuffix(t.name, ".strm")
	newName, err := open115.UpdateFile(ctx, fid, targetPureName)
	if err != nil {
		return fmt.Errorf("[同步] strm内视频改名失败: %v", err)
	}

	realExt := filepath.Ext(newName)
	if strings.TrimSuffix(newName, realExt) != targetPureName {
		_, err = open115.UpdateFile(ctx, fid, targetPureName+realExt)
		if err != nil {
			return fmt.Errorf("[同步] strm内视频二次改名失败: %v", err)
		}
	}
	if err := open115.SaveStrmFile(pickcode, fid, t.path); err != nil {
		return fmt.Errorf("[同步] strm文件写入失败: %v", err)
	}
	db.SaveRecord(t.path, fid, time.Now().Unix())
	return nil
}
func cloudScan(ctx context.Context, currentPath string) {
	select {
	case <-ctx.Done():
		return
	case sem <- struct{}{}:
		defer func() { <-sem }()
	}
	fid := db.GetFid(currentPath)
	slog.Info("[同步] 获取云端列表", "路径", currentPath)
	list, err := open115.FileList(ctx, fid)
	if err != nil {
		localCancelFunc(fmt.Errorf("获取云端列表[%s]失败: %w", currentPath, err))
		return
	}

	for _, item := range list {
		if err := context.Cause(ctx); err != nil {
			return
		}
		itemFid := item.Get("fid").String()
		if item.Get("aid").String() != "1" {
			continue
		}

		fullPath := currentPath + "/" + item.Get("fn").String()
		if item.Get("fc").String() == "0" {
			db.SaveRecord(fullPath, itemFid, -1)
			wg.Go(func() {
				cloudScan(ctx, fullPath)
			})
			continue
		}
		savePath := fullPath
		saveSize := item.Get("fs").Int()
		if item.Get("isv").Int() == 1 {
			savePath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
			saveSize = 0
			if content, err := os.ReadFile(savePath); err == nil {
				if _, localFid := extractPickcode(string(content)); localFid == itemFid {
					if info, err := os.Stat(savePath); err == nil {
						saveSize = info.ModTime().Unix()
					}
				}
			}
		}
		db.SaveRecord(savePath, itemFid, saveSize)
	}
}
func cloudSync(ctx context.Context, cloudPath string, cloudFID string) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return
	}

	slog.Info("[同步] 获取文件夹信息", "路径", cloudPath)
	_, count, folderCount, err := open115.FolderInfo(ctx, cloudPath)
	if err != nil {
		slog.Error("[同步] 获取文件夹信息失败", "路径", cloudPath, "错误信息", err)
		return
	}
	cloudTotal := count + folderCount
	dbTotal := db.GetTotalCount(cloudPath)
	if cloudTotal == dbTotal {
		return
	}
	slog.Info("[同步] 数量不一致", "路径", cloudPath, "云端数量", cloudTotal, "数据库数量", dbTotal)
	items, err := open115.FileList(ctx, cloudFID)
	if err != nil {
		slog.Error("[同步] 获取文件列表失败", "路径", cloudPath, "错误信息", err)
		return
	}

	for _, item := range items {
		if err := context.Cause(ctx); err != nil {
			return
		}
		fid := item.Get("fid").String()
		pc := item.Get("pc").String()
		if item.Get("aid").String() != "1" {
			continue
		}

		cloudPath := cloudPath + "/" + item.Get("fn").String()
		if item.Get("fc").String() == "0" {
			if db.GetFid(cloudPath) == "" {
				_ = os.MkdirAll(cloudPath, 0755)
				db.SaveRecord(cloudPath, fid, -1)
			}
			wg.Go(func() {
				cloudSync(ctx, cloudPath, fid)
			})
			continue
		}

		isStrm := item.Get("isv").Int() == 1
		savedPath := cloudPath
		saveSize := item.Get("fs").Int()

		if isStrm {
			savedPath = strings.TrimSuffix(cloudPath, filepath.Ext(cloudPath)) + ".strm"
			saveSize = time.Now().Unix()
		}

		dbFid := db.GetFid(savedPath)
		if dbFid != "" && dbFid != fid {
			slog.Info("[同步] 清理云端冗余项", "路径", savedPath)
			stats.total.Add(1)
			if err := cloudCleanTask(ctx, savedPath); err != nil {
				markFailed(fmt.Sprintf("[同步] [%s] 清理云端冗余项失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
			}
			continue
		}
		if dbFid != "" {
			continue
		}
		stats.total.Add(1)
		if isStrm {
			open115.SaveStrmFile(pc, fid, savedPath)
			slog.Info("[同步] 新增STRM文件", "路径", savedPath)
		} else {
			if err := open115.DownloadFile(ctx, pc, savedPath); err != nil {
				markFailed(fmt.Sprintf("[同步] [%s] 下载文件失败: %s (%v)", time.Now().Format("15:04"), savedPath, err))
				continue
			}
			slog.Info("[同步] 下载文件成功", "路径", savedPath)
		}
		db.SaveRecord(savedPath, fid, saveSize)
		stats.completed.Add(1)
	}
}
func cloudCleanTask(ctx context.Context, fPath string) error {
	fid, size := db.GetInfo(fPath)
	isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")

	var err error
	if isStrm || size == -1 {
		slog.Info("移动云端文件到临时目录", "路径", fPath)
		err = open115.MoveFile(ctx, fid, tempFid)
	} else {
		slog.Info("删除云端文件", "路径", fPath)
		err = open115.DeleteFile(ctx, fid)
	}

	if err != nil {
		if strings.Contains(err.Error(), "不存在或已经删除") {
			slog.Warn("云端文件不存在", "路径", fPath)
		} else {
			slog.Error("清理云端文件失败", "路径", fPath, "错误", err)
			return err
		}
	}

	db.ClearPath(fPath)
	return nil
}
func addCloudFolder(ctx context.Context, currentCID, name, fullPath string) (fid string, err error) {
	slog.Info("创建云端文件夹", "路径", fullPath)
	if fid, err = open115.AddFolder(ctx, currentCID, name); err == nil {
		return
	}
	if strings.Contains(err.Error(), "该目录名称已存在") {
		slog.Warn("云端文件夹已存在", "路径", fullPath, "错误信息", err)
		if fid, _, _, err = open115.FolderInfo(ctx, fullPath); err == nil {
			return
		} else {
			err = fmt.Errorf("获取文件夹信息失败[%s]: %w", fullPath, err)
		}
	} else {
		err = fmt.Errorf("创建云端文件夹[%s]失败: %w", fullPath, err)
	}
	return
}
func extractPickcode(content string) (pickcode, fid string) {
	u, err := url.Parse(strings.TrimSpace(content))
	if err != nil {
		return "", ""
	}
	pickcode = u.Query().Get("pickcode")
	fid = u.Query().Get("fid")
	return
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
