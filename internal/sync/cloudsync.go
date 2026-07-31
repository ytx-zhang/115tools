// cloudsync.go 实现「云端 → 本地」方向的一次性全量同步任务。
//
// 用 WalkCloud 遍历云端目录树：本地缺失的目录创建、缺失的文件下载/生成 .strm、
// 云端冗余副本清理。由 Syncer.StartTask("sync") 触发，进度经 Task 上报。
package sync

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/db"
	"log/slog"
	"os"
	"time"
)

// runCloudSync 执行一轮完整云端同步（在 Task 的协程中运行）。
func runCloudSync(ctx context.Context, env *Env, task *Task) {
	start := time.Now()
	defer func() {
		slog.Info("云端同步任务结束", "总数", task.Total(), "耗时", time.Since(start))
	}()
	slog.Info("开始同步云端文件...")

	_ = env.WalkCloud(ctx, env.Paths.SyncPath, env.Paths.SyncFid, Visitor{
		SkipByCount: true, // 计数跳过优化：没变化的目录整棵跳过，大库二次同步提速明显
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if env.DB.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					slog.Error("创建目录失败", "文件", path, "错误", err)
					return false, nil
				}
				env.DB.SaveRecord(path, fid, db.SizeDir)
				slog.Info("创建本地目录", "路径", path)
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
						slog.Error("清理云端冗余项失败", "文件", savePath, "错误", err)
					} else {
						slog.Info("删除云端冗余项", "路径", savePath, "云端FID", fid, "耗时", time.Since(t0))
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
}
