package syncFile

import (
	"115tools/db"
	"115tools/open115"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	localCancelFunc context.CancelCauseFunc
)

type uploadTask struct {
	path   string
	cid    string
	name   string
	size   int64
	isStrm bool
}

func localSync(parentCtx context.Context, workPath, workFid string) error {
	syncCond.L.Lock()
	for stats.running.Load() {
		slog.Warn("云端任务运行中,挂起等待...", "同步路径", workPath)
		syncCond.Wait()
	}
	syncCond.L.Unlock()
	var ctx context.Context
	ctx, localCancelFunc = context.WithCancelCause(parentCtx)
	defer localCancelFunc(nil)

	var uploadTasks []uploadTask
	var deleteTasks []string
	if err := localScan(ctx, workPath, workFid, &uploadTasks, &deleteTasks); err != nil {
		return err
	}
	if len(uploadTasks) == 0 && len(deleteTasks) == 0 {
		return nil
	}
	var wg sync.WaitGroup
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
				slog.Error("本地文件同步失败", "路径", t.path, "错误信息", err)
			} else {
				slog.Info("本地文件同步成功", "路径", t.path)
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

	f, err := os.Open(workPath)
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	f.Close()
	if err != nil {
		return err
	}
	localFiles := make(map[string]struct{}, len(names))
	for _, name := range names {
		localFiles[name] = struct{}{}
	}

	db.ScanChildren(ctx, workPath, func(name string, valStr string) error {
		_, exists := localFiles[name]
		fullPath := filepath.Join(workPath, name)

		if !exists {
			*deleteTasks = append(*deleteTasks, fullPath)
			return nil
		}

		delete(localFiles, name)

		var dbFid string
		var dbSize int64 = -2
		if before, after, ok := strings.Cut(valStr, "|"); ok {
			dbFid = before
			dbSize, _ = strconv.ParseInt(after, 10, 64)
		}

		info, err := os.Lstat(fullPath)
		if err != nil {
			return nil
		}

		if info.IsDir() {
			if dbSize == -1 {
				return localScan(ctx, fullPath, dbFid, uploadTasks, deleteTasks)
			}
		}

		isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
		var currentSize int64
		if isStrm {
			currentSize = info.ModTime().Unix()
		} else {
			currentSize = info.Size()
		}

		if dbSize != -1 && currentSize != dbSize {
			*deleteTasks = append(*deleteTasks, fullPath)
			*uploadTasks = append(*uploadTasks, uploadTask{
				path:   fullPath,
				cid:    workFid,
				name:   name,
				size:   currentSize,
				isStrm: isStrm,
			})
		}
		return nil
	})

	for name := range localFiles {
		fullPath := filepath.Join(workPath, name)

		info, err := os.Lstat(fullPath)
		if err != nil {
			continue
		}

		if info.IsDir() {
			fid, err := addCloudFolder(ctx, workFid, name, fullPath)
			if err != nil {
				return fmt.Errorf("创建[%s]失败: %s", fullPath, err)
			}
			if err := localScan(ctx, fullPath, fid, uploadTasks, deleteTasks); err != nil {
				return err
			}
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
		size := info.Size()
		if isStrm {
			size = info.ModTime().Unix()
		}

		*uploadTasks = append(*uploadTasks, uploadTask{
			path:   fullPath,
			cid:    workFid,
			name:   name,
			size:   size,
			isStrm: isStrm,
		})
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
			if err := cloudCleanTask(ctx, savePath); err != nil {
				return fmt.Errorf("[%s]清理过时视频失败: %s", savePath, err)
			}
		}
		if err := open115.SaveStrmFile(pickcode, fid, savePath); err != nil {
			return fmt.Errorf("[%s]写入strm文件失败: %s", savePath, err)
		}
		if err := os.Remove(t.path); err != nil {
			return fmt.Errorf("[%s]删除原文件失败: %s", t.path, err)
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
		return fmt.Errorf("STRM文件无pickcode")
	}
	if fid == "" {
		cloudFid, _, _, err := open115.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return fmt.Errorf("获取strm内视频fid失败: %v", err)
		}
		fid = cloudFid
	}
	if fid == db.GetFid(t.path) {
		db.SaveRecord(t.path, fid, t.size)
		return nil
	}

	if err := open115.MoveFile(ctx, fid, t.cid); err != nil {
		return fmt.Errorf("移动strm内视频失败: %v", err)
	}

	targetPureName := strings.TrimSuffix(t.name, ".strm")
	newName, err := open115.UpdateFile(ctx, fid, targetPureName)
	if err != nil {
		return fmt.Errorf("strm内视频改名失败: %v", err)
	}

	realExt := filepath.Ext(newName)
	if strings.TrimSuffix(newName, realExt) != targetPureName {
		_, err = open115.UpdateFile(ctx, fid, targetPureName+realExt)
		if err != nil {
			return fmt.Errorf("strm内视频二次改名失败: %v", err)
		}
	}
	if err := open115.SaveStrmFile(pickcode, fid, t.path); err != nil {
		return fmt.Errorf("strm文件写入失败: %v", err)
	}
	db.SaveRecord(t.path, fid, time.Now().Unix())
	return nil
}
func addCloudFolder(ctx context.Context, currentCID, name, fullPath string) (fid string, err error) {
	slog.Info("创建云端文件夹", "路径", fullPath)
	if fid, err = open115.AddFolder(ctx, currentCID, name); err == nil {
		db.SaveRecord(fullPath, fid, -1)
		return
	}

	if strings.Contains(err.Error(), "该目录名称已存在") {
		slog.Warn("云端文件夹已存在", "路径", fullPath, "错误信息", err)
		if fid, _, _, err = open115.FolderInfo(ctx, fullPath); err == nil {
			db.SaveRecord(fullPath, fid, -1)
			return
		} else {
			err = fmt.Errorf("获取文件夹信息失败[%s]: %w", fullPath, err)
		}
	} else {
		err = fmt.Errorf("创建云端文件夹[%s]失败: %w", fullPath, err)
	}
	return
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

	if err != nil && !strings.Contains(err.Error(), "不存在或已经删除") {
		return err
	}

	db.ClearPath(fPath)
	return nil
}
