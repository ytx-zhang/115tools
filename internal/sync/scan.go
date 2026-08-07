package sync

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/logs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 本文件实现目录级对比：与数据库记录比对，找出待上传的新文件和待清理的已删项。

// FullScan 对主同步目录做一次完整递归同步（首启收敛/定时兜底）。
// 云端同步（cloudTask）进行中时直接跳过，避免并发操作云端文件导致冲突。
func (l *instance) FullScan(ctx context.Context) {
	if l.cloudTask.Status().Running {
		logs.Info(logs.ModuleSync, "云端同步正在进行，跳过全量扫描")
		return
	}
	if l.env.Paths.SyncFid == "" {
		logs.Warn(logs.ModuleSync, "主同步目录云端FID未就绪，跳过全量扫描", "路径", l.env.Paths.SyncPath)
		return
	}
	start := time.Now()
	logs.Info(logs.ModuleSync, "开始全量本地扫描", "路径", l.env.Paths.SyncPath)
	// 深层孤儿检测：递归扫描前一次性清理（每个全量扫描周期只跑一次，不在 scanDir 递归体内重复）。
	// 孤儿记录对应的云端文件已在父目录删除时清理过，这里仅清除 DB 脏记录无需再调云端 API。
	if orphans := l.env.DB.FindOrphanSubdirs(l.env.Paths.SyncPath); len(orphans) > 0 {
		logs.Info(logs.ModuleSync, "检测到深层孤儿DB记录", "数量", len(orphans))
		l.env.DB.BatchClearPaths(orphans)
	}
	l.syncDir(ctx, l.env.Paths.SyncPath, true)
	logs.Info(logs.ModuleSync, "全量本地扫描完成", "路径", l.env.Paths.SyncPath, "耗时", time.Since(start).String())
}

// syncDir 同步一个目录：扫描差异 → 并发上传（信号量限并发）→ 全部完成才返回。幂等。
// 目录内并发、目录间串行：wg.Wait 保证本目录上传完才返回，调用方（processFolders 串行
// 遍历一批目录）因此自然「传完一个目录再扫下一个」。recursive=true 时整棵子树视为一个目录。
// ⚠️ dirMu 全局互斥：FullScan 与 watcher 并发时同一时刻只跑一个 syncDir（避免跨目录双传，
// 无需 inFlight）。代价：FullScan 持锁等整树传完期间 watcher 实时入库停摆（有 cron FullScan 兜底）。
// 目录级变更触发频繁 → 完成日志用 Debug。
func (l *instance) syncDir(ctx context.Context, currentPath string, recursive bool) {
	l.dirMu.Lock()
	defer l.dirMu.Unlock()

	start := time.Now()
	uploaded := 0
	defer func() {
		// 递归扫描（FullScan 整树）逐目录打 → Debug 防刷屏；非递归（watcher 单目录）→ Info
		if recursive {
			logs.Debug(logs.ModuleSync, "同步本地目录", "路径", currentPath, "上传文件", uploaded, "耗时", time.Since(start))
		} else {
			logs.Info(logs.ModuleSync, "同步本地目录", "路径", currentPath, "上传文件", uploaded, "耗时", time.Since(start))
		}
	}()
	uploadPaths := l.scanDir(ctx, currentPath, recursive)
	if len(uploadPaths) == 0 {
		return
	}
	// 信号量（uploadSem）限并发：与实例共享，全局上传并发上限保持 uploadWorkerCount。
	// ctx 取消时不再占槽位，doUpload 也因 ctx.Err() 快速退出，wg.Wait 不会拖住关闭流程。
	var wg sync.WaitGroup
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			break
		}
		cid := l.env.DB.GetFid(filepath.Dir(fPath))
		if cid == "" {
			logs.Warn(logs.ModuleSync, "无法获取父目录FID", "路径", fPath)
			continue
		}
		wg.Add(1)
		uploaded++
		go func(fPath, cid string) {
			defer wg.Done()
			select {
			case l.uploadSem <- struct{}{}:
				defer func() { <-l.uploadSem }()
				l.doUpload(ctx, cid, fPath)
			case <-ctx.Done():
			}
		}(fPath, cid)
	}
	wg.Wait()
}

// scanDir 对比数据库记录与本地实际内容，返回待上传文件列表。
// 两步：① DB 子项中本地已删→待清理，都在→比对内容；② 本地新增项→建云端目录/列入上传。
func (l *instance) scanDir(ctx context.Context, currentPath string, recursive bool) []string {
	logs.Debug(logs.ModuleSync, "扫描本地文件", "处理目录", currentPath)
	start := time.Now()
	defer func() {
		logs.Debug(logs.ModuleSync, "本地文件扫描完成", "处理目录", currentPath, "耗时", time.Since(start))
	}()

	if err := ctx.Err(); err != nil {
		return nil
	}

	localFiles, err := readLocalDir(currentPath)
	if err != nil {
		if os.IsNotExist(err) {
			// 本地目录在扫描中途被删除（并发/嵌套目录整体删除）：
			// 子树通常由上层清理逻辑统一处理，这里兜底再清理一次（幂等）。
			logs.Debug(logs.ModuleSync, "本地目录已不存在，兜底清理云端残留", "路径", currentPath)
			if cerr := l.cloudCleanTask(ctx, []string{currentPath}, currentPath); cerr != nil {
				logs.Debug(logs.ModuleSync, "本地目录已删除，兜底清理云端时部分项已处理", "路径", currentPath, "错误", cerr)
			}
			return nil
		}
		logs.Error(logs.ModuleSync, "读取本地目录失败", "路径", currentPath, "错误", err)
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
				uploads = append(uploads, l.scanDir(ctx, fullPath, recursive)...)
			}
			continue
		}

		fileInfo, err := localFile.Info()
		if err != nil {
			continue
		}
		localSize, refreshed := compareLocalFile(fullPath, name, dbFid, dbSize, fileInfo)
		if refreshed {
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
		logs.Error(logs.ModuleSync, "云端删除失败", "路径", currentPath, "错误", err)
	}

	// 处理本地新增项（不在数据库中的）
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return uploads
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			if _, err := AddCloudFolder(ctx, l.env, fullPath); err != nil {
				logs.Error(logs.ModuleSync, "创建云端目录失败", "路径", fullPath, "错误", err)
				continue
			}
			if recursive {
				uploads = append(uploads, l.scanDir(ctx, fullPath, recursive)...)
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
