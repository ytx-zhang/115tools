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

// 本文件实现目录级对比：与数据库记录比对，找出待上传的新文件和待清理的已删项。

// FullScan 对主同步目录做一次完整递归同步（首启收敛/定时兜底）。
// 复用与监控块相同的 syncDir，保证全量与增量行为一致。
func (l *Local) FullScan(ctx context.Context) {
	if l.env.Paths.SyncFid == "" {
		slog.Warn("主同步目录云端FID未就绪，跳过全量扫描", "路径", l.env.Paths.SyncPath)
		return
	}
	l.syncDir(ctx, l.env.Paths.SyncPath, l.env.Paths.SyncFid, true)
}

// syncDir 同步一个目录：扫描差异 → 投递待上传文件到队列（异步）。幂等。
// recursive=true 递归子树（全量）；false 只扫直属子项（监控）。
func (l *Local) syncDir(ctx context.Context, currentPath string, currentFid string, recursive bool) {
	// 阶段一：扫描（比对数据库与本地，找出待上传文件）
	uploadPaths := l.scanDir(ctx, currentPath, currentFid, recursive)
	if len(uploadPaths) == 0 {
		return
	}

	// 阶段二：把待上传文件逐个投递到上传队列（异步，3 个 worker 消费）
	// 注意：这一步只是把任务塞进 channel，立即返回，不阻塞等真正上传完成，
	// 故此处不计时——真实上传耗时由上传 worker（doUpload）单独记录。
	uploaded := 0
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			break // 取消 → 停止投递，但仍输出本次已处理的结果
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
// recursive=true 递归子目录；false 只扫直属子项（新子目录仍建好云端目录写回 FID）。
func (l *Local) scanDir(ctx context.Context, currentPath, currentFid string, recursive bool) []string {
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
	// strmRefreshes 收集「mtime 变了但 fid 一致」的 .strm：本地已知变更，需刷新数据库 size。
	// 对比循环已在读事务之外，先收集再统一写仅为减少写事务次数。
	var strmRefreshes []struct {
		path string
		fid  string
		size int64
	}

	// 第一步：一次性读取数据库该目录直属子项（单个短读事务内完成，立即关闭），
	// 之后所有对比/递归/写库都在读事务之外进行，杜绝「读事务贯穿递归」导致的写饥饿死锁。
	dbChildren := l.env.DB.ScanChildren(ctx, currentPath)

	// 第二步：拿快照比对，全部在读事务之外
	for _, ch := range dbChildren {
		if err := ctx.Err(); err != nil {
			break
		}
		name, dbFid, dbSize := ch.Name, ch.Fid, ch.Size
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			deletes = append(deletes, fullPath) // 云端存在，本地不存在 → 删除
			continue
		}
		delete(localFiles, name) // 两边都在的项从 map 移除，剩下的就是本地新增

		if localFile.IsDir() {
			if dbSize == db.SizeDir && recursive {
				// 递归下钻：此时已不在任何读事务内，子 scanDir 各自开/关自己的短读事务，无嵌套死锁。
				uploads = append(uploads, l.scanDir(ctx, fullPath, dbFid, recursive)...)
			}
			continue
		}

		// 对比文件内容是否变化
		fileInfo, err := localFile.Info()
		if err != nil {
			continue
		}
		localSize, refreshed := compareLocalFile(fullPath, name, dbFid, dbSize, fileInfo)
		if refreshed {
			// 收集刷新，循环结束后统一 SaveRecord（读事务外，安全且可批量）。
			strmRefreshes = append(strmRefreshes, struct {
				path string
				fid  string
				size int64
			}{fullPath, dbFid, localSize})
			continue
		}
		if localSize >= 0 && localSize != dbSize {
			deletes = append(deletes, fullPath)
			uploads = append(uploads, fullPath)
		}
	}

	// .strm mtime 变更刷新（fid 一致）：读事务之外统一写库，避免长事务/嵌套写锁。
	for _, r := range strmRefreshes {
		l.env.DB.SaveRecord(r.path, r.fid, r.size)
	}

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
// .strm 文件用修改时间（Unix 秒）代替文件大小；普通文件直接返回字节数。
// 返回 -1 表示文件不可读。fileInfo 由调用方通过 DirEntry.Info() 提供，避免重复 os.Stat。
//
// 第二个返回值 refreshed 标记：当 .strm 的修改时间变了、但内容里的 fid 与数据库一致时，
// 视为本地已知变更（例如 STRM 生成模块刚重写了它），应当刷新数据库记录的 size 为新的 mtime。
// 本函数不写库——调用方在「读事务之外」的对比循环里收集这些项，循环结束后统一 SaveRecord，
// 既能避免长事务，又能把多次刷新合并为批量写，减少写事务次数。
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
		_, fid := core.ExtractPickcode(fullPath)
		if fid == dbFid {
			// 本地已知变更：标记需刷新数据库 size，调用方在事务外统一 SaveRecord。
			return localSize, true
		}
	}
	return localSize, false
}

// readLocalDir 读取目录内容到 map，key 为文件名，供快速查找比对。
func readLocalDir(path string) (map[string]os.DirEntry, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	m := make(map[string]os.DirEntry, len(entries))
	for _, e := range entries {
		// 上传排除名单（下载器/系统临时文件）：命中则跳过，不进待上传/比对候选，
		// 且云端已存在的同名项会被 scanDir 判为「本地已删」而联动清理。
		if core.IsUploadExcluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}
