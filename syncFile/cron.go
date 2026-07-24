package syncFile

import (
	"context"
	"log/slog"
	"time"
)

// cronSync 定时全量同步（常驻协程，ctx 取消时退出）。
//
// 每 12 小时做两件事：
//  1. 把主同步目录登记进本地同步队列（兜底文件监听可能漏掉的本地变化）；
//  2. 启动一轮云端全量同步（拉取云端在其他设备上产生的新文件）。
//
// 两个方向各自由 local/cloud 模块执行，这里只做触发，不管过程。
func (s *SyncFile) cronSync(ctx context.Context) {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			slog.Debug("触发定时全量同步任务")
			s.local.Enqueue(s.env.Paths.SyncPath)
			s.cloud.Start(ctx)
		case <-ctx.Done():
			return
		}
	}
}
