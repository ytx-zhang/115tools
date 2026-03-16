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
		dir := filepath.Clean(filepath.Dir(event.Path)) // 规范化路径

		watchMu.Lock()

		// 1. 判断是否已经是现有任务的子目录
		isSubDir := false
		for existingPath := range watchTasks {
			// 如果 dir 是 existingPath 的子路径，或者是同一目录
			if dir == existingPath || isChildPath(existingPath, dir) {
				isSubDir = true
				break
			}

			// 2. 反向检查：如果新路径是现有路径的父目录，则移除旧的细分任务
			if isChildPath(dir, existingPath) {
				delete(watchTasks, existingPath)
			}
		}

		if !isSubDir {
			watchTasks[dir] = struct{}{}
		}

		watchMu.Unlock()

		if !watchTaskRunning.Load() && len(watchTasks) > 0 {
			if watchTaskRunning.CompareAndSwap(false, true) {
				mainWg.Go(func() { runWatchTask(ctx) })
			}
		}
	}
}
func isChildPath(parent, child string) bool {
	if len(child) <= len(parent) {
		return false
	}
	// 确保前缀匹配且紧跟路径分隔符，防止 /tmp/a 匹配 /tmp/ab
	return child[:len(parent)] == parent && child[len(parent)] == os.PathSeparator
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
			if ctx.Err() != nil {
				return
			}
			syncLocker.RLock()
			select {
			case <-ctx.Done():
				syncLocker.RUnlock()
				return
			default:
				fid := db.GetFid(path)
				if fid != "" {
					if _, err := os.Stat(path); err == nil {
						localSync(ctx, path, fid)
					}
				}
				syncLocker.RUnlock()
			}
		}
	}
}
