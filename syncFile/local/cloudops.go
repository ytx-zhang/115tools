package local

import (
	"115tools/db"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
)

// moveChunk 单次 MoveFile 请求的视频 FID 上限，避免逗号串过长。
const moveChunk = 500

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
//   - .strm 文件 → 数据库里存的就是云端视频 FID，直接 MoveFile 到 TempFid，永久保留反悔余地；
//   - 目录 → 先递归把目录下所有 .strm 对应的云端视频 MoveFile 到 TempFid，
//     再 DeleteFile 让目录（连同其余普通文件/子目录壳）进 115 回收站；
//   - 普通文件 → 直接 DeleteFile 进 115 回收站。
//
// 先把视频挪走再删目录，保证「有价值视频」落在自己管理的 TempFid（不随回收站过期），
// 而目录外壳/strm 指针/普通文件自然进 115 回收站限期清理，TempFid 不会被目录树污染。
// 最后统一清理数据库记录（BatchClearPaths 连子项一起删）。workPath 仅用于错误定位。
func (l *Local) cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	if len(fPaths) == 0 {
		return nil
	}

	var moveFids []string   // 要挪到 TempFid 的云端视频 FID
	var deleteFids []string // 直接删（进 115 回收站）的 FID：普通文件 + 目录

	// appendMove 去重追加，避免目录递归与单 strm 文件重复同一视频 FID
	appendMove := func(fid string) {
		if fid != "" && !slices.Contains(moveFids, fid) {
			moveFids = append(moveFids, fid)
		}
	}

	for _, fPath := range fPaths {
		fid, size := l.env.DB.GetInfo(fPath)
		if fid == "" {
			// 云端没记录，说明之前没同步成功过，跳过
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		if isStrm {
			// strm 文件：数据库存的 fid 就是云端视频，直接挪 TempFid
			appendMove(fid)
		} else if size == db.SizeDir {
			// 目录：先把目录下所有 strm 对应的云端视频挪 TempFid，目录本身稍后删
			for _, vf := range l.env.DB.ListStrmFids(fPath) {
				appendMove(vf)
			}
			deleteFids = append(deleteFids, fid)
		} else {
			// 普通文件：直接删（进回收站）
			deleteFids = append(deleteFids, fid)
		}
	}

	joined := strings.Join(fPaths, ",")

	// 1. 批量移动云端视频到临时目录（分批，避免单次请求过长）
	if len(moveFids) > 0 {
		for start := 0; start < len(moveFids); start += moveChunk {
			end := min(start+moveChunk, len(moveFids))
			chunk := moveFids[start:end]
			slog.Info("移动云端视频到临时目录", "路径", joined, "数量", len(chunk))
			if err := l.env.API.MoveFile(ctx, strings.Join(chunk, ","), l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", workPath, err)
			}
		}
	}

	// 2. 批量删除（普通文件 + 目录，进 115 回收站）
	if len(deleteFids) > 0 {
		slog.Info("删除云端项(进回收站)", "路径", joined, "数量", len(deleteFids))
		if err := l.env.API.DeleteFile(ctx, strings.Join(deleteFids, ",")); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", workPath, err)
		}
	}

	// 3. 清理数据库记录（连子项一起清）
	slog.Debug("清理数据库索引", "路径", joined, "数量", len(fPaths))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}
