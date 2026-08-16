package cloudsync

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// Task 云端→本地同步任务（连续性任务，由顶层 Runner 的 cloudTask 驱动）。
// 职责：遍历云端 SyncPath 树，新文件落地（视频写 strm/普通下载）、云端冗余项去重删除、
// 云端缺失目录在本地补建。是用户设计的核心（清理云端重复文件 + 下载云端多出文件）。
type Task struct {
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	wk    *Walker
	strm  *StrmIO
}

// NewTask 构造云端同步任务（依赖注入）。
func NewTask(api *drive.Client, db *store.Store, paths *common.Paths, wk *Walker, strm *StrmIO) *Task {
	return &Task{api: api, db: db, paths: paths, wk: wk, strm: strm}
}

// Run 执行一轮完整云端同步（在 Task.Start 的协程中运行）。
func (t *Task) Run(ctx context.Context, task *common.Task) {
	start := time.Now()
	defer func() {
		logs.Info(logs.ModuleSync, "云端同步完成", "路径", t.paths.SyncPath, "总数", task.Total(), "耗时", time.Since(start))
	}()
	logs.Info(logs.ModuleSync, "开始云端同步", "路径", t.paths.SyncPath)

	err := t.wk.Walk(ctx, t.paths.SyncPath, t.paths.SyncFid, common.Visitor{
		SkipByCount: true,
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			if t.db.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					logs.Error(logs.ModuleSync, "创建目录失败", "路径", path, "错误", err)
					return false, nil
				}
				t.db.SaveRecord(path, fid, store.SizeDir)
				logs.Debug(logs.ModuleSync, "创建本地目录", "路径", path)
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e common.Entry) error {
			savePath, saveSize := common.ProcessCloudFile(path, e)

			dbFid := t.db.GetFid(savePath)
			if dbFid != "" {
				if dbFid != fid {
					t0 := time.Now()
					if err := t.api.DeleteFile(ctx, fid); err != nil {
						logs.Error(logs.ModuleSync, "清理云端冗余项失败", "路径", savePath, "错误", err)
					} else {
						logs.Debug(logs.ModuleSync, "删除云端冗余项", "路径", savePath, "云端FID", fid, "耗时", time.Since(t0))
					}
				}
				return nil
			}
			task.AddTotal(1)
			if err := t.strm.FetchAndSave(ctx, logs.ModuleSync, pickCode, savePath, e.IsVideo); err != nil {
				return nil
			}
			t.db.SaveRecord(savePath, fid, saveSize)
			task.AddCompleted(1)
			return nil
		},
	}, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		logs.Error(logs.ModuleSync, "云端同步遍历失败", "路径", t.paths.SyncPath, "错误", err)
	}
}
