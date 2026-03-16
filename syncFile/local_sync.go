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

func localSync(ctx context.Context, workPath string, workFid string) {
	start := time.Now()
	defer func() {
		slog.Info("本地文件同步完成", "目录", workPath, "耗时", time.Since(start))
	}()
	var uploadPaths []string
	localScan(ctx, workPath, workFid, &uploadPaths)
	if len(uploadPaths) == 0 {
		return
	}
	uploadChan := make(chan string, len(uploadPaths))
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			for fPath := range uploadChan {
				if err := ctx.Err(); err != nil {
					return
				}
				fileInfo, err := os.Stat(fPath)
				if err != nil {
					slog.Warn("同步的文件不存在", "文件", fPath)
					continue
				}

				cid := db.GetFid(filepath.Dir(fPath))
				isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")

				if isStrm {
					err = upStrmTask(ctx, cid, fPath)
				} else {
					err = upFileTask(ctx, cid, fPath, fileInfo)
				}
				if err != nil {
					slog.Error("同步失败", "文件", fPath, "错误", err)
				} else {
					slog.Info("同步成功", "文件", fPath)
				}

			}
		})
	}
	for _, fPath := range uploadPaths {
		uploadChan <- fPath
	}
	close(uploadChan)
	wg.Wait()
}

func localScan(ctx context.Context, workPath string, workFid string, uploadTasks *[]string) {
	if err := ctx.Err(); err != nil {
		return
	}
	entries, err := os.ReadDir(workPath)
	if err != nil {
		slog.Error(err.Error())
		return
	}
	localFiles := make(map[string]os.DirEntry, len(entries))
	for _, entry := range entries {
		localFiles[entry.Name()] = entry
	}
	var deleteFilePaths []string
	var toUploadTasks []string

	db.ScanChildren(ctx, workPath, func(name string, valStr string) {
		if err := ctx.Err(); err != nil {
			return
		}
		entry, exists := localFiles[name]
		fullPath := filepath.Join(workPath, name)
		//云端存在,本地不存在
		if !exists {
			deleteFilePaths = append(deleteFilePaths, fullPath)
			return
		}

		delete(localFiles, name)

		var dbFid string
		var dbSize int64 = -2
		if before, after, ok := strings.Cut(valStr, "|"); ok {
			dbFid = before
			dbSize, _ = strconv.ParseInt(after, 10, 64)
		}

		if entry.IsDir() {
			if dbSize == -1 {
				localScan(ctx, fullPath, dbFid, uploadTasks)
			}
			return
		}

		info, err := entry.Info()
		if err != nil {
			slog.Error(err.Error())
			return
		}

		isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
		var currentSize int64
		if isStrm {
			currentSize = info.ModTime().Unix()
		} else {
			currentSize = info.Size()
		}
		//文件大小不匹配的文件
		if dbSize != -1 && currentSize != dbSize {
			deleteFilePaths = append(deleteFilePaths, fullPath)
			toUploadTasks = append(toUploadTasks, fullPath)
		}
	})
	//批量删除文件
	if err := cloudCleanTask(ctx, deleteFilePaths, workPath); err == nil {
		//上传任务
		*uploadTasks = append(*uploadTasks, toUploadTasks...)
	} else {
		slog.Error(err.Error())
	}

	//本地新增
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return
		}
		fullPath := filepath.Join(workPath, name)

		if entry.IsDir() {
			fid, err := addCloudFolder(ctx, workFid, name, fullPath)
			if err == nil {
				localScan(ctx, fullPath, fid, uploadTasks)
			} else {
				slog.Error(err.Error())
			}
			continue
		}
		*uploadTasks = append(*uploadTasks, fullPath)
	}
}
func upFileTask(ctx context.Context, cid, fPath string, fileInfo os.FileInfo) error {
	fid, pickcode, err := open115.UploadFile(ctx, fPath, cid, "", "")
	if err != nil {
		return err
	}
	size := fileInfo.Size()
	savePath := fPath
	ext := filepath.Ext(fPath)
	isVideo := checkVideo(ext, size)
	if isVideo {
		savePath = fPath[:len(fPath)-len(ext)] + ".strm"
		indexFid := db.GetFid(savePath)
		if indexFid != "" {
			if err := open115.MoveFile(ctx, indexFid, tempFid); err != nil {
				return fmt.Errorf("[%s]: 清理旧视频失败: %w", savePath, err)
			}
		}
		if err := open115.SaveStrmFile(pickcode, fid, savePath); err != nil {
			return fmt.Errorf("[%s]: 写入strm文件失败: %w", savePath, err)
		}
		if err := os.Remove(fPath); err != nil {
			return fmt.Errorf("[%s]: 删除视频文件失败: %w", fPath, err)
		}
		size = time.Now().Unix()
	}
	db.SaveRecord(savePath, fid, size)
	return nil
}
func upStrmTask(ctx context.Context, cid, fPath string) error {
	contentBytes, _ := os.ReadFile(fPath)
	pickcode, fid := extractPickcode(string(contentBytes))
	if pickcode == "" {
		return fmt.Errorf("[%s]: 无pickcode", fPath)
	}
	if fid == "" {
		cloudFid, _, _, err := open115.GetDownloadUrl(ctx, pickcode, "")
		if err != nil {
			return fmt.Errorf("[%s]: 获取fid失败: %w", fPath, err)
		}
		fid = cloudFid
	}

	if err := open115.MoveFile(ctx, fid, cid); err != nil {
		return fmt.Errorf("[%s]: 移动云端视频失败: %w", fPath, err)
	}

	targetPureName := strings.TrimSuffix(filepath.Base(fPath), ".strm")
	newName, err := open115.UpdateFile(ctx, fid, targetPureName)
	if err != nil {
		return fmt.Errorf("[%s]: 云端视频改名失败: %w", fPath, err)
	}

	trueName := targetPureName + filepath.Ext(newName)
	if newName != trueName {
		_, err = open115.UpdateFile(ctx, fid, trueName)
		if err != nil {
			return fmt.Errorf("[%s]: 云端视频二次改名失败: %w", fPath, err)
		}
	}
	if err := open115.SaveStrmFile(pickcode, fid, fPath); err != nil {
		return fmt.Errorf("[%s]: 文件写入失败: %w", fPath, err)
	}
	db.SaveRecord(fPath, fid, time.Now().Unix())
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
			err = fmt.Errorf("[%s]: 获取文件夹信息失败: %w", fullPath, err)
		}
	} else {
		err = fmt.Errorf("[%s]: 创建云端文件夹失败: %w", fullPath, err)
	}
	return
}
func cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	var moveFids []string
	var deleteFids []string

	for _, fPath := range fPaths {
		fid, size := db.GetInfo(fPath)
		if fid == "" {
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		// 如果是 strm 文件或文件夹 (size == -1)，准备移动
		if isStrm || size == -1 {
			moveFids = append(moveFids, fid)
		} else {
			deleteFids = append(deleteFids, fid)
		}
	}

	// 1. 批量移动
	if len(moveFids) > 0 {
		fidsJoin := strings.Join(moveFids, ",")
		slog.Info("批量移动云端文件到临时目录", "处理目录", workPath, "数量", len(moveFids))
		err := open115.MoveFile(ctx, fidsJoin, tempFid)
		if err != nil && !strings.Contains(err.Error(), "不存在或已经删除") {
			return fmt.Errorf("[%s]: 批量移动目录内文件失败: %w", workPath, err)
		}
	}

	// 2. 批量删除
	if len(deleteFids) > 0 {
		fidsJoin := strings.Join(deleteFids, ",")
		slog.Info("批量删除云端文件", "处理目录", workPath, "数量", len(deleteFids))
		err := open115.DeleteFile(ctx, fidsJoin)
		if err != nil && !strings.Contains(err.Error(), "不存在或已经删除") {
			return fmt.Errorf("[%s]: 批量删除目录内文件失败: %w", workPath, err)
		}
	}

	// 3. 清理数据库记录
	db.BatchClearPaths(fPaths)

	return nil
}
