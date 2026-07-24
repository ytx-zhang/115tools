package local

import (
	"115tools/db"
	"115tools/syncFile/core"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件实现「目录级全量对比」：递归对比数据库记录与本地文件系统，
// 找出需要上传的新文件和需要云端清理的已删项。

// syncDir 同步一个目录：先扫描出差异，再把需要上传的文件逐个投递到上传队列。
// 上传本身是异步的（3 个常驻 worker 消费），本函数只负责「调度」。
func (l *Local) syncDir(ctx context.Context, currentPath string, currentFid string) {
	start := time.Now()
	uploadPaths := l.scanDir(ctx, currentPath, currentFid)
	if len(uploadPaths) == 0 {
		return
	}
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			return
		}
		cid := l.env.DB.GetFid(filepath.Dir(fPath))
		if cid == "" {
			slog.Warn("无法获取父目录FID", "文件", fPath)
			continue
		}
		l.uploadOneFile(ctx, cid, fPath)
	}
	slog.Info("本地目录同步已调度", "目录", currentPath, "耗时", time.Since(start), "上传任务数", len(uploadPaths))
}

// scanDir 对比一个目录的数据库记录与本地实际内容，返回需要上传的文件列表。
//
// 对比分两步：
//  1. 遍历数据库中该目录的子项：本地没有了 → 待删除；两边都在 → 进一步比对内容；
//  2. 剩下的本地新增项（数据库里没有的）：目录先建云端目录再递归，文件直接列入待上传。
func (l *Local) scanDir(ctx context.Context, currentPath, currentFid string) []string {
	slog.Debug("扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		slog.Debug("本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	if err := ctx.Err(); err != nil {
		return nil
	}

	localFiles, err := readLocalDir(currentPath)
	if err != nil {
		slog.Error("读取本地目录失败", "路径", currentPath, "错误", err)
		return nil
	}

	var deletes []string
	var uploads []string

	// 第一步：遍历数据库子项，与本地对比
	l.env.DB.ScanChildren(ctx, currentPath, func(name string, dbFid string, dbSize int64) {
		if err := ctx.Err(); err != nil {
			return
		}
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			deletes = append(deletes, fullPath) // 云端存在，本地不存在 → 删除
			return
		}
		delete(localFiles, name) // 两边都在的项从 map 移除，剩下的就是本地新增

		if localFile.IsDir() {
			if dbSize == db.SizeDir {
				uploads = append(uploads, l.scanDir(ctx, fullPath, dbFid)...)
			}
			return
		}

		// 对比文件内容是否变化
		fileInfo, err := localFile.Info()
		if err != nil {
			return
		}
		localSize := compareLocalFile(l.env.DB, fullPath, name, dbFid, dbSize, fileInfo)
		if localSize >= 0 && localSize != dbSize {
			deletes = append(deletes, fullPath)
			uploads = append(uploads, fullPath)
		}
	})

	// 云端删除与数据库清理（本地已删的项）
	if err := l.cloudCleanTask(ctx, deletes, currentPath); err != nil {
		slog.Error("云端删除失败", "目录", currentPath, "错误", err)
	}

	// 第二步：处理本地新增项（不在数据库中的）
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return uploads
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			fid, err := l.addCloudFolder(ctx, currentFid, name, fullPath)
			if err != nil {
				slog.Error("创建云端目录失败", "路径", fullPath, "错误", err)
				continue
			}
			uploads = append(uploads, l.scanDir(ctx, fullPath, fid)...)
		} else {
			uploads = append(uploads, fullPath)
		}
	}

	return uploads
}

// compareLocalFile 返回本地文件用于和数据库对比的「大小值」。
// .strm 文件用修改时间（Unix 秒）代替文件大小；普通文件直接返回字节数。
// 返回 -1 表示文件不可读。fileInfo 由调用方通过 DirEntry.Info() 提供，避免重复 os.Stat。
//
// .strm 特殊逻辑：若只是修改时间变了、但内容里的 fid 与数据库一致，
// 视为本地已知变更（例如 STRM 生成模块刚重写了它），直接更新数据库记录
// 并返回原 dbSize——让调用方判定为「无变化」，不触发删除+重新上传。
func compareLocalFile(boltDB *db.DB, fullPath, name, dbFid string, dbSize int64, fileInfo os.FileInfo) int64 {
	if fileInfo == nil {
		return -1
	}
	isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
	if !isStrm {
		return fileInfo.Size()
	}
	localSize := fileInfo.ModTime().Unix()
	if localSize != dbSize {
		_, fid := core.ExtractPickcode(fullPath)
		if fid == dbFid {
			boltDB.SaveRecord(fullPath, fid, localSize)
			// 返回 dbSize 使调用方判定「无变化」，不触发删除+重新上传
			return dbSize
		}
	}
	return localSize
}

// readLocalDir 读取目录内容到 map，key 为文件名，供快速查找比对。
func readLocalDir(path string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		m[e.Name()] = e
	}
	return m, nil
}
