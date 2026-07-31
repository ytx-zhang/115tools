package local

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
func (l *Local) watchPump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithSeverity(fswatcher.SeverityNone), // 关闭 fswatcher 内部日志
		fswatcher.WithCooldown(0),                      // 关闭库内默认去抖，拿到持续写入的原始事件
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", l.env.Paths.SyncPath)

	// 待处理目录集合（仅本协程，mu 保护）：按目录去重。任意事件（含 rename）只取
	// 父目录加入集合；全局防抖计时器由集合中所有目录的最后一个事件重置，直到全部
	// 静默 Debounce 秒才发 wake 统一处理本轮所有目录（不再排序，顺序不影响正确性）。
	var mu sync.Mutex
	pending := make(map[string]struct{})
	wake := make(chan struct{}, 1) // 全局静默到期信号（缓冲，不保证一一对应）

	// 预建复用型全局防抖计时器：全程只分配一次，后续只 Reset，避免持续写入时
	// 每次事件都新建 Timer 给 GC 加压。回调幂等——多触发一次只多发一次 wake，
	// processReady 会清空 pending，多跑一轮只是空扫，无害。
	gTimer := time.AfterFunc(time.Hour, func() {
		select {
		case wake <- struct{}{}:
		default: // 缓冲已满则下次 processReady 顺带扫到
		}
	})
	gTimer.Stop() // 先停，避免 Reset 前自行触发

	// arm 把事件父目录加入待处理集合（去重），并重置全局防抖计时器。
	arm := func(dir string) {
		mu.Lock()
		pending[dir] = struct{}{}
		mu.Unlock()
		gTimer.Stop()
		gTimer.Reset(l.env.Paths.Debounce) // 复用同一 Timer，零分配
	}

	// processReady 取出本轮所有待处理目录（仅去重，不排序），逐个同步。
	// 处理在单协程主循环内进行：处理期间目录又来新事件会被 arm 重新登记，
	// 待其再次静默后纳入下一轮，避免并发处理同一目录。
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
			// 任何事件都只记其父目录，无视类型（rename 也走 syncDir）
			arm(filepath.Dir(ev.Path))
		case <-wake:
			processReady()
		}
	}
}

// processFolders 处理本轮所有待处理目录（缺失的父目录自动云端创建）。
// 不自行中断：syncDir 跑到底（幂等）。
func (l *Local) processFolders(ctx context.Context, folders []string) {
	for _, f := range folders {
		// 本地已不存在的目录不要据此重建云端：嵌套目录整体删除时，子目录的删除事件
		// 会把每一层都登记为待处理；若此时父层清理已清掉 DB 记录并删了云端目录，
		// 这里会误判为「新目录」而把云端文件夹「复活」。直接跳过即可，
		// 真正的云端清理由父目录那一轮 processFolders 统一完成。
		if _, statErr := os.Stat(f); statErr != nil {
			slog.Debug("待处理目录本地已不存在，跳过", "路径", f, "错误", statErr)
			continue
		}
		fid := l.env.DB.GetFid(f)
		if fid == "" {
			// 父目录在云端/数据库均无记录：自动创建（含缺失的祖先目录）后再同步。
			// AddCloudFolder 从云端根逐级确认、已存在则复用 FID，
			// 即使只监控到最深层目录、祖先未被事件触发，也不会漏传。
			var err error
			fid, err = AddCloudFolder(ctx, l.env, "", f)
			if err != nil {
				slog.Error("自动创建云端目录失败，跳过", "路径", f, "错误", err)
				continue
			}
			l.env.DB.SaveRecord(f, fid, db.SizeDir)
		}
		// 非递归：只处理本目录直接子项，子目录交给它们各自的事件，避免重复下钻。
		l.syncDir(ctx, f, fid, false)
	}
}
