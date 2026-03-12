package syncFile

import (
	"115tools/db"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sgtdi/fswatcher"
)

var (
	watchMu          sync.Mutex
	watchTasks       = make(map[string]struct{})
	watchTaskRunning atomic.Bool
)

func StartWatch(ctx context.Context, mainWg *sync.WaitGroup) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(syncPath),
		fswatcher.WithEventBatching(1*time.Second),
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	slog.Info("文件监听器启动", "路径", syncPath)
	mainWg.Go(func() { watcher.Watch(ctx) })

	for event := range watcher.Events() {
		dir := filepath.Dir(event.Path)

		watchMu.Lock()
		watchTasks[dir] = struct{}{}
		watchMu.Unlock()
		if !watchTaskRunning.Load() {
			watchTaskRunning.Store(true)
			mainWg.Go(func() { runWatchTask(ctx) })
		}
	}
}

func runWatchTask(ctx context.Context) {
	defer watchTaskRunning.Store(false)

	for {
		watchMu.Lock()
		if len(watchTasks) == 0 {
			watchMu.Unlock()
			return
		}
		todo := make([]string, 0, len(watchTasks))
		for p := range watchTasks {
			todo = append(todo, p)
			delete(watchTasks, p)
		}
		watchMu.Unlock()
		for _, path := range todo {
			select {
			case <-ctx.Done():
				return
			default:
				fid := db.GetFid(path)
				if fid != "" {
					if _, err := os.Stat(path); err == nil {
						localSync(ctx, path, fid)
					}
				}
			}
		}
	}
}
