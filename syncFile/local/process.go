package local

import (
	"115tools/db"
	"context"
	"log/slog"
	"os"
	"path/filepath"
)

// 本文件实现「单路径判定」：拿到一个变化路径后，决定对它做什么。

// processPath 处理单个被监听事件标记的路径：以「本地现状(os.Stat) + 数据库记录」为准，
// 判定该路径是 新增/删除/修改/无变化，并调用对应的 115 操作。
//
// 与目录全量扫描 scanDir 共用 compareLocalFile / uploadOneFile / cloudCleanTask /
// addCloudFolder 等底层原语，保证两种入口行为完全一致、互不重复，
// 且天然幂等（事件重复/迟到/乱序均安全）。
func (l *Local) processPath(ctx context.Context, path string) {
	if err := ctx.Err(); err != nil {
		return
	}

	// 采集三路事实：本地文件系统、数据库记录、父目录的云端 FID
	fsInfo, statErr := os.Stat(path)
	dbFid, dbSize := l.env.DB.GetInfo(path)
	parentFid := l.env.DB.GetFid(filepath.Dir(path))

	localExists := statErr == nil
	dbExists := dbFid != ""
	what := entryKind(localExists, fsInfo, dbSize)

	switch {
	case !localExists && !dbExists:
		// 本地没有、数据库也没有：临时文件出现后又消失，无需任何动作
		slog.Debug("判定：无需处理（本地项已消失且无云端记录）", "路径", path)
		return
	case !localExists && dbExists:
		// 本地已删除 → 云端对应项移入回收目录/删除，并清理数据库
		slog.Debug("判定：本地删除", "路径", path, "类型", what, "云端FID", dbFid)
		l.deleteCloudEntry(ctx, path)
	case localExists && !dbExists:
		// 本地新增，但父目录还没同步过（云端没有父目录可放）
		// → 把父目录登记回队列，等父目录的全量扫描把这一层收敛处理
		if parentFid == "" {
			slog.Debug("判定：本地新增，父目录尚未同步，转交父目录处理", "路径", path, "类型", what)
			l.Enqueue(filepath.Dir(path))
			return
		}
		slog.Debug("判定：本地新增", "路径", path, "类型", what, "父目录FID", parentFid)
		l.createLocalEntry(ctx, parentFid, filepath.Base(path), path, fsInfo)
	default: // localExists && dbExists
		if fsInfo.IsDir() {
			if dbSize != db.SizeDir {
				// 原来是文件、现在变成了目录：先清掉旧的云端文件，再按新目录处理
				slog.Debug("判定：类型变更（原文件现目录），重建", "路径", path, "类型", what)
				l.deleteCloudEntry(ctx, path)
				l.createLocalEntry(ctx, parentFid, filepath.Base(path), path, fsInfo)
			} else {
				// 目录两边都在：继续核对其子项（复用 scanDir 递归对比）
				slog.Debug("判定：目录核对子项", "路径", path, "云端FID", dbFid)
				l.syncDir(ctx, path, dbFid)
			}
		} else {
			// 文件两边都在：比对大小/修改时间，变了才重新上传
			slog.Debug("判定：文件核对变更", "路径", path, "云端FID", dbFid, "本地大小", fsInfo.Size(), "云端大小", dbSize)
			l.compareAndUploadFile(ctx, parentFid, path, dbFid, dbSize, fsInfo)
		}
	}
}

// entryKind 返回用于日志描述的条目类型（文件/目录）：优先以本地 stat 为准，
// 本地不存在时以数据库记录（目录标记 db.SizeDir）推断。
func entryKind(localExists bool, fsInfo os.FileInfo, dbSize int64) string {
	if localExists && fsInfo != nil {
		if fsInfo.IsDir() {
			return "目录"
		}
		return "文件"
	}
	if dbSize == db.SizeDir {
		return "目录"
	}
	return "文件"
}

// deleteCloudEntry 移除云端对应项：strm/目录移入回收目录，普通文件直接删除；并清理数据库。
func (l *Local) deleteCloudEntry(ctx context.Context, path string) {
	if err := l.cloudCleanTask(ctx, []string{path}, filepath.Dir(path)); err != nil {
		slog.Error("云端删除失败", "路径", path, "错误", err)
	}
}

// createLocalEntry 处理本地新增项：目录递归建云端目录并扫描内容；文件直接上传。
func (l *Local) createLocalEntry(ctx context.Context, parentFid, name, path string, fsInfo os.FileInfo) {
	if fsInfo.IsDir() {
		slog.Debug("本地新增目录，创建云端目录", "路径", path, "父目录FID", parentFid)
		fid, err := l.addCloudFolder(ctx, parentFid, name, path)
		if err != nil {
			slog.Error("创建云端目录失败", "路径", path, "错误", err)
			return
		}
		// 新目录：递归扫描其内容（复用 syncDir → scanDir）
		l.syncDir(ctx, path, fid)
		return
	}
	slog.Debug("本地新增文件，上传", "路径", path, "父目录FID", parentFid)
	l.uploadOneFile(ctx, parentFid, path)
}

// compareAndUploadFile 比对已存在文件是否变化；变化则先清旧的云端项再重新上传。
func (l *Local) compareAndUploadFile(ctx context.Context, parentFid, path, dbFid string, dbSize int64, fsInfo os.FileInfo) {
	localSize := compareLocalFile(l.env.DB, path, filepath.Base(path), dbFid, dbSize, fsInfo)
	if localSize >= 0 && localSize != dbSize {
		// 内容变化 → 先清旧的云端项，再重新上传
		slog.Debug("文件内容变更，先清旧云端项再重新上传", "路径", path, "本地大小", localSize, "云端大小", dbSize)
		l.deleteCloudEntry(ctx, path)
		l.uploadOneFile(ctx, parentFid, path)
	}
}
