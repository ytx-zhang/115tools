package syncFile

import (
	"115tools/open115"
	"bytes"
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
	boltDB       *bbolt.DB
	bucketName   = []byte("FileIndex")
	dBPath       = `/app/data/files.db`
	strmUrl      string
	tempFid      string
	doneTasks    atomic.Int32
	failedTasks  atomic.Int32
	isRunning    atomic.Bool
	needRetry    atomic.Bool
	wg           sync.WaitGroup
	scanSem      = make(chan struct{}, 3)
	cancelFunc   context.CancelFunc
	localMapPool = sync.Pool{
		New: func() any { return make(map[string]bool, 256) },
	}
	dbMapPool = sync.Pool{
		New: func() any { return make(map[string]string, 256) },
	}
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

// 获取FID
func dbGetFID(localPath string) string {
	var fid string
	boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		v := b.Get([]byte(localPath))
		if v == nil {
			return nil
		}
		s := string(v)
		if before, _, ok := strings.Cut(s, "|"); ok {
			fid = before
		}
		return nil
	})
	return fid
}

// 更新记录
func dbSaveRecord(localPath string, fid string, size int64) {
	val := fid + "|" + strconv.FormatInt(size, 10)

	boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		return b.Put([]byte(localPath), []byte(val))
	})
}

// 获取直属子文件
func dbListChildren(currentLocalPath string, res map[string]string) {
	prefix := currentLocalPath + "/"
	prefixBytes := []byte(prefix)

	boltDB.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketName).Cursor()
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			remain := k[len(prefixBytes):]
			if !bytes.Contains(remain, []byte("/")) {
				res[string(remain)] = string(v)
			}
		}
		return nil
	})
}

// 删除数据库记录
func dbClearPath(fPath string) {
	prefix := fPath + "/"
	prefixBytes := []byte(prefix)

	err := boltDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketName)
		b.Delete([]byte(fPath))

		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); {
			if err := c.Delete(); err != nil {
				return err
			}
			k, _ = c.Next()
		}
		return nil
	})

	if err != nil {
		log.Printf("[数据库] 清理路径失败 %s: %v", fPath, err)
	}
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
	rootPath := config.SyncPath
	strmUrl = config.StrmUrl
	doneTasks.Store(0)
	failedTasks.Store(0)
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
	rootID := dbGetFID(rootPath)
	// 初次运行，需初始化云端数据库
	if rootID == "" {
		log.Printf("[同步] 初次运行开始初始化云端数据库...")
		fid, err := open115.FolderInfo(ctx, rootPath)
		if err != nil {
			log.Printf("[同步] 获取根目录信息失败: %v", err)
			return
		}
		dbSaveRecord(rootPath, fid, -1)
		rootID = fid
		isScanning.Store(true)
		wg.Go(func() {
			cloudScan(ctx, rootPath)
		})
		wg.Wait()
	}
	isScanning.Store(false)
	log.Printf("[同步] 云端数据库初始化完成")
	if ctx.Err() != nil {
		return
	}

	//获取temp文件夹信息
	tempPath := config.TempPath
	tempFid = dbGetFID(tempPath)
	if tempFid == "" {
		fid, err := open115.FolderInfo(ctx, tempPath)
		if err != nil {
			log.Printf("[同步] 获取temp目录信息失败: %v", err)
			return
		}
		dbSaveRecord(tempPath, fid, -1)
		tempFid = fid
	}

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
	cid := dbGetFID(currentPath)
	log.Printf("[同步] 获取云端列表:%s", currentPath)
	list, err := open115.FileList(ctx, cid)
	if err != nil {
		log.Printf("[同步] 获取云端[%s]失败: %v", currentPath, err)
		cancelFunc()
		return
	}
	for _, item := range list {
		if ctx.Err() != nil {
			return
		}
		if item.Aid != "1" { // 有效文件
			continue
		}
		savePath := currentPath + "/" + item.Fn
		saveSize := item.Fs
		if item.Fc == "0" { // 目录
			saveSize = -1
			wg.Go(func() {
				cloudScan(ctx, savePath)
			})
		}
		// 视频文件生成 strm 记录
		if item.Isv == 1 {
			savePath = savePath[:len(savePath)-len(filepath.Ext(savePath))] + ".strm"
			saveSize = 0
			// 检查本地是否存在且匹配
			if info, err := os.Stat(savePath); err == nil {
				if content, err := os.ReadFile(savePath); err == nil {
					_, localFid := extractPickcode(string(content))
					// 如果本地 strm 记录的 fid 和云端当前文件 fid 一致
					if localFid == item.Fid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
		}
		dbSaveRecord(savePath, item.Fid, saveSize)
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

	dbFileMap := dbMapPool.Get().(map[string]string)
	clear(dbFileMap)
	defer dbMapPool.Put(dbFileMap)
	dbListChildren(currentLocalPath, dbFileMap)

	localFound := localMapPool.Get().(map[string]bool)
	clear(localFound)
	defer localMapPool.Put(localFound)

	// 遍历本地文件
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		name := entry.Name()
		localFound[name] = true
		fullPath := currentLocalPath + "/" + name

		var dbFid string
		var dbSize int64 = -2 // -2 表示本地存在但数据库无记录
		if s, exists := dbFileMap[name]; exists {
			if before, after, ok := strings.Cut(s, "|"); ok {
				dbFid = before
				dbSize, _ = strconv.ParseInt(after, 10, 64)
			}
		}

		if entry.IsDir() { // 处理文件夹
			if dbSize == -2 {
				// 本地存在云端不存在，创建云端文件夹
				newFid, err := open115.AddFolder(ctx, currentCID, name)
				if err != nil {
					log.Printf("[同步] 创建云端文件夹[%s]失败: %v", fullPath, err)
					cancelFunc()
					return
				}
				dbSaveRecord(fullPath, newFid, -1)
				dbFid = newFid
				log.Printf("[同步] 创建云端文件夹: %s", fullPath)
			}
			// 继续递归
			syncFile(ctx, fullPath, dbFid, allFileTasks)
		} else { // 处理文件
			info, _ := entry.Info()
			isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
			size := info.Size()

			if isStrm {
				size = info.ModTime().Unix()
			}

			if dbSize == -2 || size != dbSize {
				*allFileTasks = append(*allFileTasks, task{
					ctx: ctx, path: fullPath, cid: currentCID,
					name: name, size: size, isStrm: isStrm,
				})
			}
		}
	}
	// 遍历当前文件夹数据库 查找存在但本地已删除的项
	for dbFileName, dbVal := range dbFileMap {
		if !localFound[dbFileName] {
			fullPath := currentLocalPath + "/" + dbFileName
			fid, size, _ := strings.Cut(dbVal, "|")
			isDir := size == "-1"
			isStrm := strings.EqualFold(filepath.Ext(dbFileName), ".strm")
			var err error
			if isDir || isStrm {
				err = open115.MoveFile(ctx, fid, tempFid)
			} else {
				err = open115.DeleteFile(ctx, fid)
			}
			if err != nil {
				log.Printf("[同步] 删除云端文件失败: %v", err)
				return
			}
			dbClearPath(fullPath)
			log.Printf("[同步] 删除云端文件: %s", fullPath)
		}
	}
}
func processFile(ctx context.Context, fPath, cid, name string, size int64, isStrm bool, total int32) {
	if ctx.Err() != nil {
		return
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
		if err != nil {
			log.Printf("[同步] 删除云端文件失败: %v", err)
			return
		}
		dbClearPath(indexPath)
	}
	if isStrm {
		contentBytes, _ := os.ReadFile(fPath)
		fid, err = handleStrmTask(ctx, fPath, cid, name, string(contentBytes))
		if err == nil {
			dbSaveRecord(fPath, fid, time.Now().Unix())
		}
	} else {
		var pickcode string
		fid, pickcode, err = open115.UploadFile(ctx, fPath, cid, "", "")
		if err == nil {
			if isVideo {
				baseUrl, _ := strings.CutSuffix(strmUrl, "/")
				strmContent := fmt.Sprintf("%s/download?pickcode=%s&fid=%s",
					baseUrl, pickcode, fid)

				if err := os.WriteFile(indexPath, []byte(strmContent), 0644); err == nil {
					if ctx.Err() != nil {
						return
					}
					if err := os.Remove(fPath); err == nil {
						dbSaveRecord(indexPath, fid, time.Now().Unix())
					} else {
						log.Printf("[同步] 删除原文件失败: %v", err)
						return
					}
				}
			} else {
				dbSaveRecord(fPath, fid, size)
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
