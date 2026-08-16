package sync

import (
	"context"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// cronTask 定时全量同步任务（连续性任务，由 runner.Start 启动常驻协程）。
// 职责：按配置间隔定时触发「本地全量扫描 + 云端同步」。
//
// 依赖（runner 注入）：cfg（是否启用/间隔）、runLocalSync（本地全量扫描执行器，即 r.startLocalSync）、
// startCloud（云端同步任务触发，即 r.startCloudSync）。
type cronTask struct {
	cfg          *config.Config
	runLocalSync func() // 由 runner 注入：r.startLocalSync（含 cloudTask 互斥判定 + localCtx 管理）
	startCloud   func() // 由 runner 注入：r.startCloudSync（触发云端同步任务）
}

// loop 定时全量同步。cron.enabled=false 时挂起空转。
// 输出/副作用：每间隔触发一次全量扫描 + 云端同步（由注入的执行器负责）。
func (c *cronTask) loop(ctx context.Context) {
	if !c.cfg.CronEnabled() {
		logs.Info(logs.ModuleSync, "定时全量同步已关闭，仅依赖本地文件监听")
		<-ctx.Done()
		return
	}
	interval := c.cfg.CronInterval()
	logs.Info(logs.ModuleSync, "定时全量同步已启用", "间隔", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logs.Info(logs.ModuleSync, "触发定时全量同步任务")
			c.runLocalSync()
			c.startCloud()
		case <-ctx.Done():
			return
		}
	}
}
