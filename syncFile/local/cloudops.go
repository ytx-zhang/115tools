package local

import (
	"115tools/db"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
)

// 本文件是本地同步模块专用的两个云端写操作：建目录、批量清理。
// 它们只服务于「本地 → 云端」方向的业务，因此放在本模块内而非 core。

// addCloudFolder 在云端目录 currentCID 下创建子目录 fileName，
// 并把 本地路径 → 新目录 FID 记入数据库（后续其子文件上传时要用父目录 FID）。
func (l *Local) addCloudFolder(ctx context.Context, currentCID, fileName, fullPath string) (string, error) {
	fid, err := l.env.API.AddFolder(ctx, currentCID, fileName)
	if err != nil {
		return "", fmt.Errorf("[%s]: 创建云端文件夹失败: %w", fullPath, err)
	}
	l.env.DB.SaveRecord(fullPath, fid, db.SizeDir)
	slog.Info("创建云端目录", "路径", fullPath, "云端FID", fid)
	return fid, nil
}

// cloudCleanTask 批量清理「本地已删除」路径对应的云端项，分三类处理：
//   - .strm 文件 / 目录 → 移入回收目录（TempFid），保留反悔余地；
//   - 普通文件 → 直接删除（它们本来就是从本地上传的，本地没了云端也没必要留）；
//   - 最后统一清理数据库记录。
//
// workPath 仅用于错误信息定位（调用方所在目录）。
func (l *Local) cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	if len(fPaths) == 0 {
		return nil
	}
	var moveFids []string
	var deleteFids []string

	for _, fPath := range fPaths {
		fid, size := l.env.DB.GetInfo(fPath)
		if fid == "" {
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		// strm 文件或目录 → 移动；普通文件 → 删除
		if isStrm || size == db.SizeDir {
			moveFids = append(moveFids, fid)
		} else {
			deleteFids = append(deleteFids, fid)
		}
	}

	joined := strings.Join(fPaths, ",")

	// 1. 批量移动到回收目录
	if len(moveFids) > 0 {
		fidsJoin := strings.Join(moveFids, ",")
		slog.Info("移动云端项到临时目录", "路径", joined, "数量", len(moveFids))
		err := l.env.API.MoveFile(ctx, fidsJoin, l.env.Paths.TempFid)
		if err != nil {
			return fmt.Errorf("[%s]: 批量移动目录内文件失败: %w", workPath, err)
		}
	}

	// 2. 批量删除
	if len(deleteFids) > 0 {
		fidsJoin := strings.Join(deleteFids, ",")
		slog.Info("删除云端项", "路径", joined, "数量", len(deleteFids))
		err := l.env.API.DeleteFile(ctx, fidsJoin)
		if err != nil {
			return fmt.Errorf("[%s]: 批量删除目录内文件失败: %w", workPath, err)
		}
	}

	// 3. 清理数据库记录
	slog.Debug("清理数据库索引", "路径", joined, "数量", len(fPaths))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}
