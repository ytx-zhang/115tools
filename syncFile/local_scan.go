package syncFile

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"115tools/db"
)

// ──── 本地同步主流程 ────

func (s *SyncFile) localSync(ctx context.Context, currentPath string, currentFid string) {
	start := time.Now()
	uploadPaths := s.localScan(ctx, currentPath, currentFid)
	if len(uploadPaths) == 0 {
		return
	}
	for _, fPath := range uploadPaths {
		if err := ctx.Err(); err != nil {
			return
		}
		cid := s.db.GetFid(filepath.Dir(fPath))
		if cid == "" {
			slog.Warn("无法获取父目录FID", "文件", fPath)
			continue
		}
		s.uploadOneFile(ctx, cid, fPath)
	}
	slog.Info("本地目录同步已调度", "目录", currentPath, "耗时", time.Since(start), "上传任务数", len(uploadPaths))
}

// ──── 目录扫描：对比 DB 与本地文件系统 ────

func (s *SyncFile) localScan(ctx context.Context, currentPath, currentFid string) []string {
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

	// 遍历 DB 子项，与本地对比
	s.db.ScanChildren(ctx, currentPath, func(name string, dbFid string, dbSize int64) {
		if err := ctx.Err(); err != nil {
			return
		}
		localFile, exists := localFiles[name]
		fullPath := filepath.Join(currentPath, name)

		if !exists {
			deletes = append(deletes, fullPath) // 云端存在，本地不存在 → 删除
			return
		}
		delete(localFiles, name)

		if localFile.IsDir() {
			if dbSize == db.SizeDir {
				uploads = append(uploads, s.localScan(ctx, fullPath, dbFid)...)
			}
			return
		}

		// 对比文件是否变化
		fileInfo, err := localFile.Info()
		if err != nil {
			return
		}
		localSize := compareLocalFile(s.db, fullPath, name, dbFid, dbSize, fileInfo)
		if localSize >= 0 && localSize != dbSize {
			deletes = append(deletes, fullPath)
			uploads = append(uploads, fullPath)
		}
	})

	// 云删除与 DB 清理
	if err := s.cloudCleanTask(ctx, deletes, currentPath); err != nil {
		slog.Error("云端删除失败", "目录", currentPath, "错误", err)
	}

	// 本地新增项（不在 DB 中的）
	for name, entry := range localFiles {
		if err := ctx.Err(); err != nil {
			return uploads
		}
		fullPath := filepath.Join(currentPath, name)
		if entry.IsDir() {
			fid, err := s.addCloudFolder(ctx, currentFid, name, fullPath)
			if err != nil {
				slog.Error("创建云端目录失败", "路径", fullPath, "错误", err)
				continue
			}
			uploads = append(uploads, s.localScan(ctx, fullPath, fid)...)
		} else {
			uploads = append(uploads, fullPath)
		}
	}

	return uploads
}

// ──── 文件对比 ────

// compareLocalFile 返回本地文件用于和 DB 对比的大小值。
// .strm 文件用修改时间（Unix 秒）代替文件大小；普通文件直接返回字节数。
// 返回 -1 表示文件不可读。fileInfo 由调用方通过 DirEntry.Info() 提供，避免重复 os.Stat。
func compareLocalFile(boltDB *db.DB, fullPath, name, dbFid string, dbSize int64, fileInfo os.FileInfo) int64 {
	if fileInfo == nil {
		return -1
	}
	isStrm := strings.EqualFold(filepath.Ext(name), ".strm")
	if !isStrm {
		return fileInfo.Size()
	}
	// .strm 文件：若仅修改时间变化但 pickcode 中 fid 匹配，视为本地已知变更，
	// 更新 DB 记录（不触发重新上传）
	localSize := fileInfo.ModTime().Unix()
	if localSize != dbSize {
		_, fid := extractPickcode(fullPath)
		if fid == dbFid {
			boltDB.SaveRecord(fullPath, fid, localSize)
			// 返回 dbSize 使调用方判定“无变化”，不触发删除+重新上传
			return dbSize
		}
	}
	return localSize
}

// readLocalDir 读取目录内容到 map，key 为文件名。
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
