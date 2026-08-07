package sync

import (
	"context"
	"os"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/logs"
	"path/filepath"
	"sync"
	"time"
)

// watchPump 是文件监听器主循环（常驻协程，ctx 取消退出）。
// 无视事件类型，只记父目录 → 全局静默窗口去抖（按目录去重）→ 交由 executor 协程处理。
// 主循环只做「收事件/登记/续命计时器」，绝不执行耗时业务，保证事件即时消费不丢、防抖心跳不断。
// 云端无记录的目录先用 AddCloudFolder 补建祖先。子目录各自独立触发，避免重复扫描。
func (l *instance) watchPump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithSeverity(fswatcher.SeverityNone), // 关闭 fswatcher 内部日志
		fswatcher.WithCooldown(0),                      // 关闭库内默认去抖，拿到持续写入的原始事件
	)
	if err != nil {
		logs.Error(logs.ModuleSync, "监听器启动失败", "err", err)
		return
	}
	go func() {
		if err := watcher.Watch(ctx); err != nil {
			logs.Error(logs.ModuleSync, "监听器运行异常退出", "err", err)
		}
	}()
	logs.Info(logs.ModuleSync, "文件监听器启动", "路径", l.env.Paths.SyncPath)

	// 待处理目录集合（主循环与 executor 并发读写，mu 必须保护）：按目录去重。
	// 任意事件只取父目录加入集合；全局防抖计时器由最后一个事件重置，
	// 全部静默 Debounce 秒后 kick executor 统一处理。
	var mu sync.Mutex
	pending := make(map[string]struct{})
	kick := make(chan struct{}, 1)

	// notify 唤醒 executor。cap=1 且满了就丢——executor 每轮取走全部 pending，多余信号无意义。
	notify := func() {
		select {
		case kick <- struct{}{}:
		default:
		}
	}

	// 复用型全局防抖计时器：全程只分配一次，后续只 Reset，避免持续写入时每次事件新建 Timer。
	// 回调幂等——多触发一次多跑一轮空扫，无害。
	gTimer := time.AfterFunc(time.Hour, notify)
	gTimer.Stop()

	// arm 把事件父目录加入待处理集合（去重），并重置全局防抖计时器。
	arm := func(dir string) {
		mu.Lock()
		pending[dir] = struct{}{}
		mu.Unlock()
		gTimer.Stop()
		gTimer.Reset(l.env.Paths.Debounce)
	}

	// take 取出并清空当前待处理集合（快照后立即释放锁，处理期间事件可继续登记）。
	take := func() []string {
		mu.Lock()
		defer mu.Unlock()
		if len(pending) == 0 {
			return nil
		}
		folders := make([]string, 0, len(pending))
		for f := range pending {
			folders = append(folders, f)
		}
		clear(pending)
		return folders
	}

	// executor 常驻协程：串行处理每一批目录，与主循环解耦，处理耗时不阻塞事件消费。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-kick:
				folders := take()
				if len(folders) == 0 {
					continue
				}
				pendingDir := l.processFolders(ctx, folders)
				// 子目录已删时父目录需重新扫描：登记回 pending 并续命防抖。
				for _, d := range pendingDir {
					arm(d)
				}
				// 处理期间新登记的目录：重新走一轮防抖，避免扫到仍在写入的文件。
				mu.Lock()
				remain := len(pending)
				mu.Unlock()
				if remain > 0 {
					gTimer.Stop()
					gTimer.Reset(l.env.Paths.Debounce)
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			gTimer.Stop()
			logs.Info(logs.ModuleSync, "文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			arm(filepath.Dir(ev.Path))
		}
	}
}

// processFolders 由 executor 协程串行调用，处理一批待处理目录（缺失的父目录自动云端创建）。
// 不自行中断：syncDir 幂等跑到底。os.Stat 复活检查必须保留（解耦后处理延迟更长，本地目录被删窗口更大）。
// 返回需重新登记的父目录：子目录已删时其父目录需扫描才能发现子项缺失并清理 DB 孤儿记录。
func (l *instance) processFolders(ctx context.Context, folders []string) []string {
	// 云端同步（runCloudSync）进行中时跳过，避免 cloudCleanTask 删除/移动云端文件与
	// WalkCloud 遍历并发冲突；目录原样返回由 executor re-arm（登记+续命防抖），
	// 云端同步结束后自动补处理，不丢变更。
	if l.cloudTask.Status().Running {
		logs.Info(logs.ModuleSync, "云端同步正在进行，本地变更稍后处理", "数量", len(folders))
		return folders
	}
	// 监听触发的同步是关键提醒：即使每批触发也保持 Info，让用户看到入库在进行
	t0 := time.Now()
	processed := 0
	defer func() {
		logs.Info(logs.ModuleSync, "处理变更目录完成", "数量", len(folders), "处理", processed, "耗时", time.Since(t0))
	}()
	var retryParents []string
	for _, f := range folders {
		// 本地已不存在的目录不要据此重建云端：嵌套目录整体删除时，子目录删除事件会把每一层
		// 都登记为待处理；若此时父层清理已清 DB 并删了云端目录，这里会误判「新目录」而复活云端。
		// ⚠️ 子目录已删时，必须把父目录重新加入待处理：只有父目录扫描才能发现子项缺失并清理 DB。
		if _, statErr := os.Stat(f); statErr != nil {
			logs.Debug(logs.ModuleSync, "待处理目录本地已不存在，跳过", "路径", f, "错误", statErr)
			if f != l.env.Paths.SyncPath {
				if parent := filepath.Dir(f); parent != "." {
					retryParents = append(retryParents, parent)
				}
			}
			continue
		}
		if l.env.DB.GetFid(f) == "" {
			if _, err := AddCloudFolder(ctx, l.env, f); err != nil {
				logs.Error(logs.ModuleSync, "自动创建云端目录失败，跳过", "路径", f, "错误", err)
				continue
			}
		}
		logs.Info(logs.ModuleSync, "处理变更目录", "路径", f)
		l.syncDir(ctx, f, false)
		processed++
	}
	return retryParents
}
