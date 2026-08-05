package sync

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/logs"
	"path/filepath"
	"slices"
	"strings"
)

// moveChunk 单次 MoveFile 请求的视频 FID 上限，避免逗号串过长。
const moveChunk = 500

// AddCloudFolder 确保云端目录存在并返回 FID。纯云端操作，不写库（FID 由调用方落库）。
// currentCID 非空时直接在其下建末级目录（扫描热路径，单步）；
// 为空时从根 "0" 逐级 GetDirInfo 确认、缺失再建（初始化/watcher 补建祖先用）。
func AddCloudFolder(ctx context.Context, env *Env, currentCID, path string) (string, error) {
	if currentCID != "" {
		fid, err := env.API.AddFolder(ctx, currentCID, filepath.Base(path))
		if err != nil {
			return "", fmt.Errorf("[%s]: 创建云端文件夹失败: %w", path, err)
		}
		return fid, nil
	}

	// 起点：115 根目录 FID 恒为 "0"，无需经 get_info 反推。
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue
		}
		cur = cur + "/" + seg
		if info, err := env.API.GetDirInfo(ctx, cur); err == nil {
			parentFid = info.Fid
			continue
		}
		fid, err := env.API.AddFolder(ctx, parentFid, seg)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
	}
	return parentFid, nil
}

// cloudCleanTask 批量清理本地已删路径对应的云端项：
// .strm→MoveFile 到 TempFid（保留视频）；目录→先搬子 .strm 视频再 DeleteFile；
// 普通文件→DeleteFile。最后 BatchClearPaths 清库。
func (l *instance) cloudCleanTask(ctx context.Context, fPaths []string, workPath string) error {
	if len(fPaths) == 0 {
		return nil
	}

	var moveFids []string
	var deleteFids []string

	appendMove := func(fid string) {
		if fid != "" && !slices.Contains(moveFids, fid) {
			moveFids = append(moveFids, fid)
		}
	}

	for _, fPath := range fPaths {
		fid, size := l.env.DB.GetInfo(fPath)
		if fid == "" {
			continue
		}

		isStrm := strings.EqualFold(filepath.Ext(fPath), ".strm")
		if isStrm {
			appendMove(fid)
		} else if size == db.SizeDir {
			for _, vf := range l.env.DB.ListStrmFids(fPath) {
				appendMove(vf)
			}
			deleteFids = append(deleteFids, fid)
		} else {
			deleteFids = append(deleteFids, fid)
		}
	}

	joined := strings.Join(fPaths, ",")

	if len(moveFids) > 0 {
		for start := 0; start < len(moveFids); start += moveChunk {
			end := min(start+moveChunk, len(moveFids))
			chunk := moveFids[start:end]
			if err := l.env.API.MoveFile(ctx, strings.Join(chunk, ","), l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", workPath, err)
			}
		}
	}

	if len(deleteFids) > 0 {
		if err := l.env.API.DeleteFile(ctx, strings.Join(deleteFids, ",")); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", workPath, err)
		}
	}

	logs.Info(logs.ModuleSync, "清理数据库索引", "路径", joined, "数量", len(fPaths))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}
