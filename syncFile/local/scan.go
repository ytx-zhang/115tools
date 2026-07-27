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
	scanStart := time.Now()
	uploadPaths := l.scanDir(ctx, currentPath, currentFid, recursive)
	slog.Info("扫描本地目录", "目录", currentPath, "需要上传文件", len(uploadPaths), "耗时", time.Since(scanStart))

	// 无变更无需进入同步阶段，扫描日志已说明
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
// 取消通过 ctx 传递，叠加到各检查点（ScanChildren 回调、新增项循环）。
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
			if dbSize == db.SizeDir && recursive {
				uploads = append(uploads, l.scanDir(ctx, fullPath, dbFid, recursive)...)
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
		// 上传排除名单（下载器/系统临时文件）：命中则跳过，不进待上传/比对候选，
		// 且云端已存在的同名项会被 scanDir 判为「本地已删」而联动清理。
		if core.IsUploadExcluded(e.Name()) {
			continue
		}
		m[e.Name()] = e
	}
	return m, nil
}
