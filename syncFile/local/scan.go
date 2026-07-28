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

// 本文件实现「目录级对比」：与数据库记录比对，找出需要上传的新文件和需要云端清理的已删项。
// 全量扫描/定时兜底时 recursive=true 递归下钻整棵子树；监控触发时 recursive=false，只扫本目录直接子项。

// FullScan 对主同步目录做一次完整递归同步（扫描块入口）。
// 用于：程序启动后的首次收敛、定时全量扫描兜底。复用与监控块完全相同的 syncDir，
// 保证全量扫描与实时增量行为完全一致。主目录云端 FID 未就绪时跳过（不应发生）。
// 全量扫描传 recursive=true，递归下钻整棵子树。
func (l *Local) FullScan(ctx context.Context) {
	if l.env.Paths.SyncFid == "" {
		slog.Warn("主同步目录云端FID未就绪，跳过全量扫描", "路径", l.env.Paths.SyncPath)
		return
	}
	l.syncDir(ctx, l.env.Paths.SyncPath, l.env.Paths.SyncFid, true)
}

// syncDir 同步一个目录：先扫描出差异，再把需要上传的文件逐个投递到上传队列。
// 上传本身是异步的（3 个常驻 worker 消费），本函数只负责「调度」。
// 取消通过 ctx（监控块退出、全量扫描被取消）传递，syncDir 幂等、跑到底无副作用。
//
// recursive=true 递归下钻整棵子树（全量扫描/定时兜底用）；
// recursive=false 只处理本目录的直接子项，子目录交给它们各自的事件（监控块用），
// 避免每一层目录被反复递归多次。
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

// scanDir 对比一个目录的数据库记录与本地实际内容，返回需要上传的文件列表。
//
// 对比分两步：
//  1. 遍历数据库中该目录的子项：本地没有了 → 待删除；两边都在 → 进一步比对内容；
//  2. 剩下的本地新增项（数据库里没有的）：目录先建云端目录（仅在 recursive 时递归下钻），
//     文件直接列入待上传。
//
// scanDir 扫描本地目录，与数据库比对，返回需要上传的本地文件路径列表。
// 云端目录的创建和文件的删除在这里同步完成，文件上传则交给 syncDir 异步调度。
// 取消通过 ctx 传递，叠加到各检查点（dbChildren 快照循环、新增项循环）。
//
// recursive=true 时对子目录继续递归下钻（全量扫描/定时兜底）；
// recursive=false 时只处理本目录直接子项，新子目录仍会建好云端目录并写回 FID（供其子目录
// 各自的事件使用），但不再下钻——交给监控块各自的 processReady 处理，避免重复扫描整棵子树。
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
