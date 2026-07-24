// Package cloud 是「云端同步」模块：负责把 115 云端的文件同步到本地
// （云端 → 本地 的下载方向）。
//
// 【做什么】
// 从主同步目录（SyncPath）出发，用 core.WalkCloud 递归遍历整棵云端目录树：
//   - 遇到本地没有的目录 → 在本地创建；
//   - 遇到本地没有的文件 → 普通文件下载、视频生成 .strm 索引；
//   - 遇到数据库记录与云端 FID 不一致的项 → 说明云端出现了重复/过期副本，删除冗余项。
//
// 【怎么触发】
// 由 web 面板手动触发（POST /api/task/sync）、或每 12 小时的定时全量同步触发；
// Start 自带防重入（同一时间只跑一轮），Stop 用于面板手动停止。
//
// 【与其他模块的边界】
// 与 local 模块不加锁互斥，靠幂等避让：本模块「先写文件、紧接着写数据库」，
// 待 local 模块处理对应文件事件时数据库记录已就位，判定为「无变化」，
// 不会把刚下载的文件又反向上传回云端。
package cloud

import (
	"115tools/db"
	"115tools/syncFile/core"
	"context"
	"fmt"
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
// notify 是状态变更通知通道，由 Runner 创建并跨热重载共享（见 core.TaskStats）。
// 调用方：syncFile 根包的 New()。
func New(env *core.Env, notify chan struct{}) *Cloud {
	return &Cloud{
		env:   env,
		stats: core.NewTaskStats(notify),
	}
}

// StatusJSON 返回任务进度的 JSON 快照，供根包拼装面板状态。
func (c *Cloud) StatusJSON() string {
	return c.stats.GetStatus()
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

	// 遍历整棵云端目录树。致命错误已由回调内的 FailLog 逐条记录，
	// WalkCloud 的返回值（仅 GetFileList 致命失败）这里显式忽略。
	_ = c.env.WalkCloud(ctx, c.env.Paths.SyncPath, c.env.Paths.SyncFid, core.Visitor{
		SkipByCount: true, // 计数跳过优化：没变化的目录整棵跳过，大库二次同步提速明显
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			// 数据库里没有的目录 = 云端新增目录 → 本地创建并记录
			if c.env.DB.GetFid(path) == "" {
				if err := os.MkdirAll(path, 0755); err != nil {
					core.FailLog(&c.stats, path, "创建目录失败", err)
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
					if err := c.env.API.DeleteFile(ctx, fid); err != nil {
						core.FailLog(&c.stats, savePath, "清理云端冗余项失败", err)
					} else {
						slog.Info("删除云端冗余项", "路径", savePath, "云端FID", fid)
					}
				}
				return nil
			}
			// 本地没有的新文件：下载（视频则生成 .strm），成功后记录数据库
			c.stats.AddTotal(1)
			if err := c.env.FetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo, &c.stats); err != nil {
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
