package sync

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/db"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 本文件实现目录级对比：与数据库记录比对，找出待上传的新文件和待清理的已删项。

// FullScan 对主同步目录做一次完整递归同步（首启收敛/定时兜底）。
func (l *instance) FullScan(ctx context.Context) {
	if l.env.Paths.SyncFid == "" {
		slog.Warn("主同步目录云端FID未就绪，跳过全量扫描", "路径", l.env.Paths.SyncPath)
		return
	}
	l.syncDir(ctx, l.env.Paths.SyncPath, l.env.Paths.SyncFid, true)
}

// syncDir 同步一个目录：扫描差异 → 投递待上传文件到队列（异步）。幂等。
// recursive=true 递归子树（全量）；false 只扫直属子项（监控）。
func (l *instance) syncDir(ctx context.Context, currentPath, currentFid string, recursive bool) {
	uploadPaths := l.scanDir(ctx, currentPath, currentFid, recursive)
	if len(uploadPaths) == 0 {
		return
	}
	uploaded := 0
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			break
		}
		cid := l.env.DB.GetFid(filepath.Dir(fPath))
		if cid == "" {
			slog.Warn("无法获取父目录FID", "文件", fPath)
			continue
		}
		l.uploadOneFile(ctx, cid, fPath)
		uploaded++
	}
	slog.Info("同步本地目录", "目录", currentPath, "上传文件", uploaded)
}

// scanDir 对比数据库记录与本地实际内容，返回待上传文件列表。
// 两步：① DB 子项中本地已删→待清理，都在→比对内容；② 本地新增项→建云端目录/列入上传。
func (l *instance) scanDir(ctx context.Context, currentPath, currentFid string, recursive bool) []string {
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
		if os.IsNotExist(err) {
			// 本地目录在扫描中途被删除（并发/嵌套目录整体删除）：
			// 子树通常由上层清理逻辑统一处理，这里兜底再清理一次（幂等）。
			slog.Debug("本地目录已不存在，兜底清理云端残留", "路径", currentPath)
			if cerr := l.cloudCleanTask(ctx, []string{currentPath}, currentPath); cerr != nil {
				slog.Debug("本地目录已删除，兜底清理云端时部分项已处理", "目录", currentPath, "错误", cerr)
			}
			return nil
		}
		slog.Error("读取本地目录失败", "路径", currentPath, "错误", err)
		return nil
	}

	var deletes []string
	var uploads []string

	// 一次性读取数据库该目录直属子项（单个短读事务内完成，立即关闭），
	// 之后所有对比/递归/写库都在读事务之外进行，杜绝「读事务贯穿递归」导致的写饥饿死锁。
	dbChildren := l.env.DB.ScanChildren(ctx, currentPath)

	for _, ch := range dbChildren {
		if err := ctx.Err(); err != nil {
			break
		}
		name, dbFid, dbSize := ch.Name, ch.Fid, ch.Size
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			deletes = append(deletes, fullPath)
			continue
		}
		delete(localFiles, name)

		if localFile.IsDir() {
			if dbSize == db.SizeDir && recursive {
				uploads = append(uploads, l.scanDir(ctx, fullPath, dbFid, recursive)...)
			}
			continue
		}

		fileInfo, err := localFile.Info()
		if err != nil {
			continue
		}
		localSize, refreshed := compareLocalFile(fullPath, name, dbFid, dbSize, fileInfo)
		if refreshed {
			// .strm mtime 变了但 fid 一致：读事务之外直接刷新数据库 size（安全）。
			l.env.DB.SaveRecord(fullPath, dbFid, localSize)
			continue
		}
		if localSize >= 0 && localSize != dbSize {
			deletes = append(deletes, fullPath)
			uploads = append(uploads, fullPath)
		}
	}

	// 云端删除与数据库清理（本地已删的项）
	if err := l.cloudCleanTask(ctx, deletes, currentPath); err != nil {
		slog.Error("云端删除失败", "目录", currentPath, "错误", err)
	}

	// 处理本地新增项（不在数据库中的）
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return uploads
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			fid, err := AddCloudFolder(ctx, l.env, currentFid, fullPath)
			if err != nil {
				slog.Error("创建云端目录失败", "路径", fullPath, "错误", err)
				continue
			}
			l.env.DB.SaveRecord(fullPath, fid, db.SizeDir)
			if recursive {
				uploads = append(uploads, l.scanDir(ctx, fullPath, fid, recursive)...)
			}
		} else {
			uploads = append(uploads, fullPath)
		}
	}

	return uploads
}

// compareLocalFile 返回本地文件用于和数据库对比的「大小值」。
// .strm 用修改时间（Unix 秒）代替文件大小；普通文件直接返回字节数。返回 -1 表示不可读。
// 当 .strm 的 mtime 变了、但内容里的 fid 与数据库一致时，视为本地已知变更（如 STRM 模块刚重写），
// 第二个返回值 refreshed=true，调用方在事务外刷新数据库 size。本函数不写库。
func compareLocalFile(fullPath, name, dbFid string, dbSize int64, fileInfo os.FileInfo) (int64, bool) {
	if fileInfo == nil {
		return -1, false
	}
	isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
	if !isStrm {
		return fileInfo.Size(), false
	}
	localSize := fileInfo.ModTime().Unix()
	if localSize != dbSize {
		_, fid := ExtractPickcode(fullPath)
		if fid == dbFid {
			return localSize, true
		}
	}
	return localSize, false
}

// readLocalDir 读取目录内容到 map（文件名→DirEntry）。命中上传排除名单的项直接跳过，
// 因此云端已存在的同名项会被 scanDir 判为「本地已删」而联动清理。
func readLocalDir(path string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		if IsUploadExcluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}
