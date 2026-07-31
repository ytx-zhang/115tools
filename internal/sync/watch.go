package sync

import (
	"context"
	"os"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/db"
	"log/slog"
	"path/filepath"
	"sync"
	"time"
)

// watchPump 是文件监听器主循环（常驻协程，ctx 取消退出）。
// 无视事件类型，只记父目录 → 全局静默窗口去抖（按目录去重）→ 统一处理本轮目录。
// 云端无记录的目录先用 AddCloudFolder 补建祖先。子目录各自独立触发，避免重复扫描。
func (l *instance) watchPump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithSeverity(fswatcher.SeverityNone), // 关闭 fswatcher 内部日志
		fswatcher.WithCooldown(0),                      // 关闭库内默认去抖，拿到持续写入的原始事件
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go func() {
		if err := watcher.Watch(ctx); err != nil {
			slog.Error("[监听器] 运行异常退出", "err", err)
		}
	}()
	slog.Info("文件监听器启动", "路径", l.env.Paths.SyncPath)

	// 待处理目录集合（仅本协程，mu 保护）：按目录去重。任意事件只取父目录加入集合；
	// 全局防抖计时器由集合中最后一个事件重置，直到全部静默 Debounce 秒才发 wake 统一处理。
	var mu sync.Mutex
	pending := make(map[string]struct{})
	wake := make(chan struct{}, 1)

	// 复用型全局防抖计时器：全程只分配一次，后续只 Reset，避免持续写入时每次事件新建 Timer。
	// 回调幂等——多触发一次多跑一轮空扫，无害。
	gTimer := time.AfterFunc(time.Hour, func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	})
	gTimer.Stop()

	// arm 把事件父目录加入待处理集合（去重），并重置全局防抖计时器。
	arm := func(dir string) {
		mu.Lock()
		pending[dir] = struct{}{}
		mu.Unlock()
		gTimer.Stop()
		gTimer.Reset(l.env.Paths.Debounce)
	}

	processReady := func() {
		mu.Lock()
		folders := make([]string, 0, len(pending))
		for f := range pending {
			folders = append(folders, f)
		}
		pending = make(map[string]struct{})
		mu.Unlock()
		l.processFolders(ctx, folders)
	}

	for {
		select {
		case <-ctx.Done():
			gTimer.Stop()
			slog.Info("文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			arm(filepath.Dir(ev.Path))
		case <-wake:
			processReady()
		}
	}
}

// processFolders 处理本轮所有待处理目录（缺失的父目录自动云端创建）。不自行中断：syncDir 幂等跑到底。
func (l *instance) processFolders(ctx context.Context, folders []string) {
	for _, f := range folders {
		// 本地已不存在的目录不要据此重建云端：嵌套目录整体删除时，子目录删除事件会把每一层
		// 都登记为待处理；若此时父层清理已清 DB 并删了云端目录，这里会误判「新目录」而复活云端。
		if _, statErr := os.Stat(f); statErr != nil {
			slog.Debug("待处理目录本地已不存在，跳过", "路径", f, "错误", statErr)
			continue
		}
		fid := l.env.DB.GetFid(f)
		if fid == "" {
			var err error
			fid, err = AddCloudFolder(ctx, l.env, "", f)
			if err != nil {
				slog.Error("自动创建云端目录失败，跳过", "路径", f, "错误", err)
				continue
			}
			l.env.DB.SaveRecord(f, fid, db.SizeDir)
		}
		l.syncDir(ctx, f, fid, false)
	}
}
