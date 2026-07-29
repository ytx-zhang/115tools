package local

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/syncFile/core"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// moveChunk 单次 MoveFile 请求的视频 FID 上限，避免逗号串过长。
const moveChunk = 500

// 本文件实现本地同步专用的两个云端写操作：建目录、批量清理。

// AddCloudFolder 确保云端目录存在并返回 FID。纯云端操作，不写库（FID 由调用方落库）。
// currentCID 非空时直接在其下建末级目录（扫描热路径，单步）；
// 为空时从根 "0" 逐级 GetDirInfo 确认、缺失再建（初始化/watcher 补建祖先用）。
func AddCloudFolder(ctx context.Context, env *core.Env, currentCID, path string) (string, error) {
	if currentCID != "" {
		fid, err := env.API.AddFolder(ctx, currentCID, filepath.Base(path))
		if err != nil {
			return "", fmt.Errorf("[%s]: 创建云端文件夹失败: %w", path, err)
		}
		slog.Info("创建云端目录", "路径", path, "云端FID", fid)
		return fid, nil
	}

	// 起点：115 根目录 FID 恒为 "0"，无需经 get_info 反推（根目录 get_info 返回子项数组）。
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue // 前导或重复的 "/" 会产生空段，跳过
		}
		cur = cur + "/" + seg
		if info, err := env.API.GetDirInfo(ctx, cur); err == nil {
			parentFid = info.Fid // 该层已存在，继续向下
			continue
		}
		fid, err := env.API.AddFolder(ctx, parentFid, seg)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		slog.Info("创建云端目录", "路径", cur, "云端FID", fid)
		parentFid = fid
	}
	return parentFid, nil
}

// cloudCleanTask 批量清理本地已删路径对应的云端项：
// .strm→MoveFile 到 TempFid（保留视频）；目录→先搬子 .strm 视频再 DeleteFile；
// 普通文件→DeleteFile。最后 BatchClearPaths 清库。
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
			t0 := time.Now()
			if err := l.env.API.MoveFile(ctx, strings.Join(chunk, ","), l.env.Paths.TempFid); err != nil {
				return fmt.Errorf("[%s]: 批量移动云端视频失败: %w", workPath, err)
			}
			slog.Info("移动云端视频到临时目录", "路径", joined, "数量", len(chunk), "耗时", time.Since(t0))
		}
	}

	// 2. 批量删除（普通文件 + 目录，进 115 回收站）
	if len(deleteFids) > 0 {
		t0 := time.Now()
		if err := l.env.API.DeleteFile(ctx, strings.Join(deleteFids, ",")); err != nil {
			return fmt.Errorf("[%s]: 批量删除云端项失败: %w", workPath, err)
		}
		slog.Info("删除云端项(进回收站)", "路径", joined, "数量", len(deleteFids), "耗时", time.Since(t0))
	}

	// 3. 清理数据库记录（连子项一起清）
	slog.Debug("清理数据库索引", "路径", joined, "数量", len(fPaths))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}
