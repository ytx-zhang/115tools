package syncFile

import (
	"115tools/config"
	"115tools/db"
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/sgtdi/fswatcher"
)

func startWatch(ctx context.Context) {
	syncPath := config.Get().SyncPath
	watcher, err := fswatcher.New(
		fswatcher.WithPath(syncPath),
		fswatcher.WithEventBatching(1*time.Second),
		fswatcher.WithCooldown(10*time.Second),
	)
	if err != nil {
		slog.Error("文件监听器初始化失败", "错误信息", err)
		return
	}
	slog.Info("文件监听器启动", "路径", syncPath)
	go watcher.Watch(ctx)
	for event := range watcher.Events() {
		if err := context.Cause(ctx); err != nil {
			return
		}
		dirPath := filepath.Dir(event.Path)
		fid := db.GetFid(dirPath)
		if fid == "" {
			continue
		}
		localSync(ctx, dirPath, fid)
	}
}
