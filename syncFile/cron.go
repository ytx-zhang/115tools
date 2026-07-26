package syncFile

import (
	"context"
	"log/slog"
	"time"
)

// cronSync 定时全量同步（常驻协程，ctx 取消时退出）。
//
// 每隔 env.CronInterval 做两件事：
//  1. 对主同步目录做一次全量递归同步（兜底文件监听可能漏掉的本地变化）；
//  2. 启动一轮云端全量同步（拉取云端在其他设备上产生的新文件）。
//
// 两个方向各自由 local/cloud 模块执行，这里只做触发，不管过程。
// 若配置 cron_enabled=false，则本协程挂起空转、不做任何定时扫描，
// 仅依赖本地文件监听同步；变更配置需热重载同步器以重建本协程。
func (s *SyncFile) cronSync(ctx context.Context) {
	if !s.env.CronEnabled {
		slog.Info("[定时] 定时全量同步已关闭（配置 cron_enabled=false），仅依赖本地文件监听")
		<-ctx.Done()
		return
	}

	interval := s.env.CronInterval
	slog.Info("[定时] 定时全量同步已启用", "间隔", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Debug("触发定时全量同步任务")
			s.local.FullScan(ctx)
			s.cloud.Start(ctx)
		case <-ctx.Done():
			return
		}
	}
}
