package syncFile

import (
	"115tools/open115"
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

var (
	boltDB      *bbolt.DB
	bucketName  = []byte("FileIndex")
	dBPath      = `/app/data/files.db`
	strmUrl     string
	rootPath    string
	tempFid     string
	doneTasks   atomic.Int32
	failedTasks atomic.Int32
	isRunning   atomic.Bool
	needRetry   atomic.Bool
	wg          sync.WaitGroup
	scanSem     = make(chan struct{}, 3)
	cancelFunc  context.CancelFunc
	logs        []string
	logsMutex   sync.Mutex
)

type task struct {
	ctx    context.Context
	path   string
	cid    string
	name   string
	size   int64
	isStrm bool
}

// --- 数据库初始化与封装 ---
func initDB() {
	var err error
	boltDB, err = bbolt.Open(dBPath, 0600, nil)
	if err != nil {
		log.Fatalf("[数据库] 初始化失败: %v", err)
	}
	boltDB.MaxBatchDelay = 100 * time.Millisecond
	boltDB.MaxBatchSize = 2000
	boltDB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucketName)
		return err
	})
}

func closeDB() {
	if boltDB != nil {
		log.Printf("[数据库] 正在关闭 bbolt...")
		boltDB.Close()
	}
}

func removeDB() {
	if err := os.Remove(dBPath); err != nil {
		log.Printf("[同步] 清理数据库失败: %v", err)
	} else {
		log.Printf("[同步] 已清理数据库文件")
	}
}

// 获取云端文件/文件夹 ID
func getCloudID(localPath string) string {
	var fid string
	p := filepath.ToSlash(localPath)
	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(p))
		if v != nil {
			parts := strings.SplitN(string(v), "|", 2)
			if len(parts) > 0 {
				fid = parts[0]
			}
		}
		return nil
	})
	return fid
}

// 更新记录
func updateIndex(localPath string, fid string, size int64) {
	p := filepath.ToSlash(localPath)
	val := fid + "|" + strconv.FormatInt(size, 10)

	boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(p), []byte(val))
	})
}

// 获取直属子文件 Map
func getFolderFileMap(currentLocalPath string) map[string]string {
	res := make(map[string]string)
	prefix := filepath.ToSlash(currentLocalPath)
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	boltDB.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		prefixBytes := []byte(prefix)
		for k, v := c.Seek(prefixBytes); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			pathStr := string(k)
			if !strings.Contains(pathStr[len(prefix):], "/") {
				res[filepath.Base(pathStr)] = string(v)
			}
		}
		return nil
	})
	return res
}

// RemoveFile 处理单个文件的删除
func removeFile(fPath string) {
	p := filepath.ToSlash(fPath)
	err := boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Delete([]byte(p))
	})

	if err != nil {
		log.Printf("[数据库] 删除记录失败: %v", err)
	}
}

// RemoveFolder 递归清理文件夹及其所有下属内容
func removeFolder(fPath string) {
	p := filepath.ToSlash(fPath)
	prefix := p + "/"
	boltDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		b.Delete([]byte(p))
		c := b.Cursor()
		for k, _ := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); {
			c.Delete()
			k, _ = c.Next()
		}
		return nil
	})
}

func StartSync(parentCtx context.Context, mainWg *sync.WaitGroup) {
	needRetry.Store(true)

	if isRunning.CompareAndSwap(false, true) {
		mainWg.Go(func() {
			defer isRunning.Store(false)
			for needRetry.Swap(false) {
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
		cancelFunc()
		log.Printf("[同步] 正在中止当前运行中的任务...")
	}
}
func runSync(parentCtx context.Context) {
	var ctx context.Context
	ctx, cancelFunc = context.WithCancel(parentCtx)
	var isScanning atomic.Bool
	config := open115.Conf.Load()
	rootPath = config.SyncPath
	strmUrl = config.StrmUrl
	doneTasks.Store(0)
	failedTasks.Store(0)
	logsMutex.Lock()
	logs = logs[:0]
	logsMutex.Unlock()
	initDB()

	defer func() {
		cancelFunc()
		closeDB()
		if isScanning.Load() {
			removeDB() // 扫描过程中被取消，删除数据库
		}
		log.Printf("[同步] 任务彻底停止")
	}()

	if rootPath == "" {
		log.Printf("[同步] SyncPath 未设置")
		return
	}

	log.Printf("[同步] 开始同步流程...")

	// 获取根目录 ID
	rootID := getCloudID(rootPath)
	// 初次运行，需初始化云端数据库
	if rootID == "" {
		log.Printf("[同步] 初次运行开始初始化云端数据库...")
		fid, err := open115.FolderInfo(ctx, rootPath)
		if err != nil {
			log.Printf("[同步] 获取根目录信息失败: %v", err)
			return
		}
		updateIndex(rootPath, fid, -1)
		rootID = fid
		isScanning.Store(true)
		wg.Go(func() {
			cloudScan(ctx, rootPath)
		})
		wg.Wait()
	}
	if ctx.Err() != nil {
		return
	}
	isScanning.Store(false)
	log.Printf("[同步] 云端数据库初始化完成")
	//获取temp文件夹信息
	tempPath := config.TempPath
	fid, err := open115.FolderInfo(ctx, tempPath)
	if err != nil {
		log.Printf("[同步] 获取temp目录信息失败: %v", err)
		return
	}
	tempFid = fid
	log.Printf("[同步] 开始扫描本地文件")
	var allFileTasks []task
	syncFile(ctx, rootPath, rootID, &allFileTasks)

	if ctx.Err() != nil {
		return
	}
	total := int32(len(allFileTasks))
	if total == 0 {
		log.Printf("[同步] 未发现新任务")
		return
	}
	log.Printf("[同步] 本地文件扫描完成，开始处理文件，总数: %d", total)
	taskQueue := make(chan task, 3)
	const workerCount = 3
	for range workerCount {
		wg.Go(func() {
			for task := range taskQueue {
				if ctx.Err() != nil {
					continue
				}
				processFile(task.ctx, task.path, task.cid, task.name, task.size, task.isStrm, total)
			}
		})
	}
	go func() {
		for _, t := range allFileTasks {
			taskQueue <- t
		}
		close(taskQueue)
	}()
	wg.Wait() // 等待所有 文件任务 退出
	log.Printf("[同步] 同步流程结束，处理文件总数: %d", total)
}

func GetStatus() (bool, int32, int32) {
	return isRunning.Load(), doneTasks.Load(), failedTasks.Load()
}

// --- 递归扫描逻辑 ---

func cloudScan(ctx context.Context, currentPath string) {
	select {
	case <-ctx.Done():
		return
	case scanSem <- struct{}{}:
		defer func() { <-scanSem }()
	}
	cid := getCloudID(currentPath)
	list, err := open115.FileList(ctx, cid)
	if err != nil {
		log.Printf("[同步] 获取云端[%s]失败: %v", currentPath, err)
		cancelFunc()
		return
	}
	log.Printf("[同步] 获取云端[%s]成功", currentPath)
	for _, item := range list {
		if ctx.Err() != nil {
			return
		}
		if item.Aid != "1" { // 有效文件
			continue
		}
		fullPath := filepath.Join(currentPath, item.Fn)
		if item.Fc == "0" { // 目录
			updateIndex(fullPath, item.Fid, -1)
			wg.Go(func() {
				cloudScan(ctx, fullPath)
			})
		} else {
			savePath := fullPath
			saveSize := item.Fs
			// 视频文件特殊处理
			if item.Isv == 1 {
				savePath = strings.TrimSuffix(fullPath, filepath.Ext(fullPath)) + ".strm"
				saveSize = 0 // STRM 文件大小记为0
			}
			updateIndex(savePath, item.Fid, saveSize)
		}
	}
}

func syncFile(ctx context.Context, currentLocalPath string, currentCID string, allFileTasks *[]task) {
	if ctx.Err() != nil {
		return
	}
	entries, err := os.ReadDir(currentLocalPath)
	if err != nil {
		log.Printf("[同步] 获取本地目录[%s]失败: %v", currentLocalPath, err)
		cancelFunc()
		return
	}

	dbFileMap := getFolderFileMap(currentLocalPath)
	localFound := make(map[string]bool)

	// 遍历本地文件
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		name := entry.Name()
		fullPath := filepath.Join(currentLocalPath, name)
		localFound[name] = true

		var dbFid string
		var dbSize int64 = -2 // -2 表示本地存在但数据库无记录
		if val, exists := dbFileMap[name]; exists {
			parts := strings.SplitN(val, "|", 2)
			dbFid = parts[0]
			dbSize, _ = strconv.ParseInt(parts[1], 10, 64)
		}

		if entry.IsDir() { // 处理文件夹
			if dbSize == -2 {
				// 本地存在云端不存在，创建云端文件夹
				newFid, err := open115.AddFolder(ctx, currentCID, name)
				if err != nil {
					log.Printf("[同步] 创建云端文件夹失败: %v", err)
					cancelFunc()
					return
				}
				updateIndex(fullPath, newFid, -1)
				dbFid = newFid
				log.Printf("[同步] 创建云端文件夹: %s", fullPath)
			}
			// 继续递归
			syncFile(ctx, fullPath, dbFid, allFileTasks)
		} else { // 处理文件
			// 逻辑判断是否需要处理文件
			info, _ := entry.Info()
			ext := strings.ToLower(filepath.Ext(name))
			isStrm := (ext == ".strm")
			size := info.Size()

			shouldProcess := false
			if isStrm {
				size = info.ModTime().Unix()
			}
			if dbSize == -2 || size != dbSize { // 新增或大小不符
				shouldProcess = true
			}

			if shouldProcess {
				task := task{
					ctx:    ctx,
					path:   fullPath,
					cid:    currentCID,
					name:   name,
					size:   size,
					isStrm: isStrm,
				}
				*allFileTasks = append(*allFileTasks, task)
			}
		}
	}
	//删除云端和数据库记录
	var deleteFids []string
	type deleteIndex struct {
		path  string
		isDir bool
	}
	var toDeleteIndex []deleteIndex
	// 遍历当前文件夹数据库 查找存在但本地已删除的项
	for dbFileName, dbVal := range dbFileMap {
		if !localFound[dbFileName] {
			fullPath := filepath.Join(currentLocalPath, dbFileName)
			parts := strings.SplitN(dbVal, "|", 2)
			fid := parts[0]
			isDir := strings.HasSuffix(dbVal, "|-1")
			deleteFids = append(deleteFids, fid)
			toDeleteIndex = append(toDeleteIndex, deleteIndex{path: fullPath, isDir: isDir})
		}
	}
	// 移动云端文件
	if len(deleteFids) > 0 {
		// 将 []string 转换为 "id1,id2,id3" 格式
		batchFids := strings.Join(deleteFids, ",")
		err := open115.MoveFile(ctx, batchFids, tempFid)
		if err != nil {
			log.Printf("[同步] 批量移动云端文件失败: %v", err)
			cancelFunc()
			return
		}
		log.Printf("[同步] 移动云端[%s]内文件数量: %d", currentLocalPath, len(deleteFids))
	}
	// 删除本地数据库记录
	if len(toDeleteIndex) > 0 {
		for _, index := range toDeleteIndex {
			if index.isDir {
				removeFolder(index.path)
			} else {
				removeFile(index.path)
			}
			log.Printf("[同步] 删除[%s]数据库索引", index.path)
		}
	}
}
func processFile(ctx context.Context, fPath, cid, name string, size int64, isStrm bool, total int32) {
	if ctx.Err() != nil {
		return
	}
	var err error
	var fid string
	path := fPath
	ext := strings.ToLower(filepath.Ext(fPath))
	isVideo := ext == ".mp4" || ext == ".mkv"
	if isVideo {
		path = strings.TrimSuffix(fPath, filepath.Ext(fPath)) + ".strm"
	}
	cloudFid := getCloudID(path)
	if cloudFid != "" {
		err := open115.MoveFile(ctx, cloudFid, tempFid)
		if err != nil {
			log.Printf("[同步] 移动云端文件失败: %v", err)
			return
		}
		removeFile(path)
		log.Printf("[同步] 删除重复文件[%s]", path)
	}
	if isStrm {
		contentBytes, _ := os.ReadFile(path)
		fid, err = handleStrmTask(ctx, path, cid, name, string(contentBytes))
		if err == nil {
			updateIndex(path, fid, time.Now().Unix())
		}
	} else {
		var pickcode string
		fid, pickcode, err = open115.UploadFile(ctx, fPath, cid, "", "")
		if err == nil {
			if isVideo {
				baseUrl, _ := strings.CutSuffix(strmUrl, "/")
				strmContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s",
					baseUrl, pickcode, fid)

				if err = os.WriteFile(path, []byte(strmContent), 0644); err == nil {
					if ctx.Err() != nil {
						return
					}
					if err = os.Remove(fPath); err == nil {
						updateIndex(path, fid, time.Now().Unix())
					}
				}
			} else {
				updateIndex(path, fid, size)
			}
		}
	}

	curr := doneTasks.Add(1)
	if err != nil {
		failedTasks.Add(1)
		log.Printf("[同步][%d/%d] ❌ %s: %v", curr, total, fPath, err)
	} else {
		log.Printf("[同步][%d/%d] ✅ %s", curr, total, fPath)
	}
}

func handleStrmTask(ctx context.Context, fPath, cid, name, content string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	pickcode, fid := extractPickcode(content)
	if pickcode == "" {
		return "", fmt.Errorf("[同步] STRM内容无pickcode")
	}
	if fid == "" {
		downloadInfo, err := open115.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return "", fmt.Errorf("[同步] 获取strm内视频fid失败: %v", err)
		}
		fid = downloadInfo.FileID
	}

	// 1. 移动文件
	if err := open115.MoveFile(ctx, fid, cid); err != nil {
		return "", fmt.Errorf("[同步] 移动strm内视频失败: %v", err)
	}

	// 2. 改名及后缀二次校验
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
	// 3. 写入文件
	baseUrl, _ := strings.CutSuffix(strmUrl, "/")
	newContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s", baseUrl, pickcode, fid)
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
