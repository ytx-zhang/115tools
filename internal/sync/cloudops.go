package sync

import (
	"context"
	"errors"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/logs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// moveChunk 单次 MoveFile 请求的视频 FID 上限，避免逗号串过长。
const moveChunk = 500

// AddCloudFolder 逐级确保云端目录存在并写入数据库。
// 每层先查 DB：已有则复用 FID；缺失则云端 AddFolder 并即时写库。
// 返回末级 FID，调用方无需再 SaveRecord。
func AddCloudFolder(ctx context.Context, env *Env, path string) (string, error) {
	parentFid := "0"
	cur := ""
	for seg := range strings.SplitSeq(path, "/") {
		if seg == "" {
			continue
		}
		cur = cur + "/" + seg
		if fid := env.DB.GetFid(cur); fid != "" {
			parentFid = fid
			continue
		}
		fid, err := env.API.AddFolder(ctx, parentFid, seg)
		if err != nil {
			return "", fmt.Errorf("创建云端目录 %s 失败: %w", cur, err)
		}
		parentFid = fid
		env.DB.SaveRecord(cur, fid, db.SizeDir)
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
	t0 := time.Now()

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

	// 目标太多（批量清理）只显示父目录与数量，避免逗号路径串刷屏
	logs.Info(logs.ModuleSync, "清理数据库索引", "目标目录", workPath, "数量", len(fPaths), "耗时", time.Since(t0))
	l.env.DB.BatchClearPaths(fPaths)

	return nil
}

// ──── 云端 → 本地全量同步 ────

// runCloudSync 执行一轮完整云端同步（在 Task 的协程中运行）。
func runCloudSync(ctx context.Context, env *Env, task *Task) {
	start := time.Now()
	defer func() {
		logs.Info(logs.ModuleSync, "云端同步任务结束", "路径", env.Paths.SyncPath, "总数", task.Total(), "耗时", time.Since(start))
	}()
	logs.Info(logs.ModuleSync, "开始同步云端文件", "路径", env.Paths.SyncPath)

	err := env.WalkCloud(ctx, env.Paths.SyncPath, env.Paths.SyncFid, Visitor{
		SkipByCount: true,
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if env.DB.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					logs.Error(logs.ModuleSync, "创建目录失败", "文件", path, "错误", err)
					return false, nil
				}
				env.DB.SaveRecord(path, fid, db.SizeDir)
				// 云端遍历期间逐目录创建 → 高频，Debug
				logs.Debug(logs.ModuleSync, "创建本地目录", "路径", path)
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e Entry) error {
			savePath, saveSize := ProcessCloudFile(path, e)

			dbFid := env.DB.GetFid(savePath)
			if dbFid != "" {
				if dbFid != fid {
					t0 := time.Now()
					if err := env.API.DeleteFile(ctx, fid); err != nil {
						logs.Error(logs.ModuleSync, "清理云端冗余项失败", "文件", savePath, "错误", err)
					} else {
						logs.Info(logs.ModuleSync, "删除云端冗余项", "路径", savePath, "云端FID", fid, "耗时", time.Since(t0))
					}
				}
				return nil
			}
			task.AddTotal(1)
			if err := env.FetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo); err != nil {
				return nil
			}
			env.DB.SaveRecord(savePath, fid, saveSize)
			task.AddCompleted(1)
			return nil
		},
	}, nil)
	// WalkCloud 返回的错误仅来自致命失败（拉列表失败/上下文取消），取消不算错误
	if err != nil && context.Cause(ctx) != nil && !errors.Is(context.Cause(ctx), context.Canceled) {
		logs.Error(logs.ModuleSync, "云端同步遍历失败", "路径", env.Paths.SyncPath, "错误", err)
	}
}
