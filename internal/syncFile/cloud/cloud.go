// Package cloud 是云端同步模块（云端 → 本地下载方向）。
// 用 WalkCloud 遍历云端目录树：新目录本地创建、新文件下载/生成 .strm、
// 冗余项清理。Start 自带防重入，与 local 模块靠幂等避让不加锁。
package cloud

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/syncFile/core"
	"log/slog"
	"os"
	"time"
)

// Cloud 是云端同步模块的实例。
// 模块无后台常驻协程——每次 Start 就是一轮完整任务，跑完即结束。
type Cloud struct {
	env    *core.Env               // 共享运行环境（API/DB/路径配置）
	stats  core.TaskStats          // 任务进度统计（总数/完成/失败/运行中），驱动前端进度条
	cancel context.CancelCauseFunc // 取消本轮任务；Stop 时以「用户请求」为原因调用
}

// New 创建云端同步模块实例。
// onChange 是状态变更回调，由 Runner 注入（见 core.TaskStats）：每次进度变化时
// 回调组装完整状态快照并广播给 web 层。调用方：syncFile 根包的 New()。
func New(env *core.Env, onChange func()) *Cloud {
	return &Cloud{
		env:   env,
		stats: core.NewTaskStats(onChange),
	}
}

// Status 返回当前进度快照。
func (c *Cloud) Status() *core.TaskProgress {
	return c.stats.Status()
}

// Start 启动一轮云端全量同步（在调用方的协程中运行，通常由 web 层异步触发）。
// 同一时刻只允许一轮任务：重复触发直接返回。
func (c *Cloud) Start(parentCtx context.Context) {
	if !c.stats.TryStart() {
		return // 已有一轮在跑，忽略本次触发
	}
	start := time.Now()
	defer func() {
		slog.Info("云端文件同步完成", "总数", c.stats.Total(), "耗时", time.Since(start))
		c.stats.SetRunning(false)
		c.cancel(nil)
	}()
	c.stats.Reset()
	ctx, cancel := context.WithCancelCause(parentCtx)
	c.cancel = cancel
	slog.Info("开始同步云端文件...")

	// 遍历整棵云端目录树。回调内已逐条 slog.Error 记录错误（云端同步不依赖失败计数，
	// 仅把错误统一进日志卡片），WalkCloud 的返回值（仅 GetFileList 致命失败）这里显式忽略。
	_ = c.env.WalkCloud(ctx, c.env.Paths.SyncPath, c.env.Paths.SyncFid, core.Visitor{
		SkipByCount: true, // 计数跳过优化：没变化的目录整棵跳过，大库二次同步提速明显
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			// 数据库里没有的目录 = 云端新增目录 → 本地创建并记录
			if c.env.DB.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					slog.Error("创建目录失败", "文件", path, "错误", err)
					return false, nil
				}
				c.env.DB.SaveRecord(path, fid, db.SizeDir)
				slog.Info("创建本地目录", "路径", path)
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e core.Entry) error {
			savePath, saveSize := core.ProcessCloudFile(path, e)

			dbFid := c.env.DB.GetFid(savePath)
			if dbFid != "" {
				// 数据库已有同路径记录：FID 一致说明是同一文件，跳过；
				// FID 不一致说明云端存在过期/重复副本，删除冗余项。
				if dbFid != fid {
					t0 := time.Now()
					if err := c.env.API.DeleteFile(ctx, fid); err != nil {
						slog.Error("清理云端冗余项失败", "文件", savePath, "错误", err)
					} else {
						slog.Info("删除云端冗余项", "路径", savePath, "云端FID", fid, "耗时", time.Since(t0))
					}
				}
				return nil
			}
			// 本地没有的新文件：下载（视频则生成 .strm），成功后记录数据库
			c.stats.AddTotal(1)
			if err := c.env.FetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo); err != nil {
				return nil
			}
			c.env.DB.SaveRecord(savePath, fid, saveSize)
			c.stats.AddCompleted(1)
			return nil
		},
	}, nil)
}

// Stop 停止正在运行的本轮同步（面板「停止」按钮）。
// 无任务在跑时安全地什么都不做。
func (c *Cloud) Stop() {
	if c.stats.Running() && c.cancel != nil {
		c.cancel(fmt.Errorf("用户请求停止同步"))
	}
}
