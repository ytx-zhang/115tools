package local

import (
	"115tools/db"
	"context"
	"log/slog"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/sgtdi/fswatcher"
)

// watchPump 是文件监听器主循环（常驻协程，ctx 取消时退出）。
//
// 监听器完全「不懂」业务逻辑，只把原始事件对应的「父目录」登记进待处理表，
// 按目录各自独立做静默窗口（目录自身 Debounce 秒内无新事件才处理），再对
// 每个待处理目录跑 syncDir。每个目录只处理自身直接子项（syncDir 非递归），
// 子目录交给它们各自的事件，避免同一棵子树被多层目录事件重复扫描。
//
// 若某目录在云端/数据库均无记录（fid==""），先用 AddCloudFolder 从云端根逐级确认、
// 自动创建（含缺失的祖先目录）并写回 FID，再同步——即便只监控到最深层目录、其祖先
// 未被事件触发，也不会漏传。
//
// 事件类型完全无视（连 rename 也走 syncDir）——换取零类型判断，代价是改名时
// 旧视频进 TempFid、新文件重传（已确认接受）。
//
// 按目录静默（非全局单计时器）的好处：目录互不干扰；同时保留「整批消停才扫」，
// 绝不扫半成品（大文件被原地拷贝时，直到它真正静默才上传）。
func (l *Local) watchPump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithSeverity(fswatcher.SeverityNone), // 关闭 fswatcher 内部日志
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", l.env.Paths.SyncPath)

	// 待处理状态（仅本协程，mu 保护）：
	//   pending：还在静默窗口内（计时器运行中）的目录
	//   ready：  静默已到期、待处理的目录
	//   timers： 每个目录的静默计时器（事件到达即重置）
	var mu sync.Mutex
	pending := make(map[string]struct{})
	ready := make(map[string]struct{})
	timers := make(map[string]*time.Timer)
	wake := make(chan struct{}, 1) // 目录静默到期信号（缓冲，不保证一一对应）

	// arm 把目录登记为「活跃」并（重）启动其静默计时器。
	// 任意事件（含 rename）只取父目录；同目录后续事件重置窗口，直到目录自身静默
	// Debounce 秒才处理。新事件同时撤销该目录可能残留的 ready 标记——目录重新
	// 活跃就不应被视为已到期。
	arm := func(dir string) {
		mu.Lock()
		pending[dir] = struct{}{}
		delete(ready, dir)
		t, ok := timers[dir]
		mu.Unlock()
		if ok {
			t.Stop() // AfterFunc 无通道，Stop 失败（已触发）即忽略
		}
		timers[dir] = time.AfterFunc(l.env.Paths.Debounce, func() {
			mu.Lock()
			if _, ok := pending[dir]; ok {
				delete(pending, dir)
				ready[dir] = struct{}{}
			}
			delete(timers, dir)
			mu.Unlock()
			select {
			case wake <- struct{}{}:
			default: // 缓冲已满则下次 processReady 顺带扫到
			}
		})
	}

	// processReady 取出所有已静默目录，逐个同步（缺失的父目录自动云端创建）。
	// 不自行中断：syncDir 跑到底（幂等）；处理期间目录又来新事件会被 arm 重新登记，
	// 待其再次静默后处理。
	processReady := func() {
		mu.Lock()
		folders := make([]string, 0, len(ready))
		for f := range ready {
			folders = append(folders, f)
		}
		ready = make(map[string]struct{})
		mu.Unlock()

		// 仅排序（父目录先于子目录），不丢弃任何目录：每个目录都只处理自身直接子项
		// （syncDir 非递归），由各自事件触发，故全部保留、仅按路径升序保证父 FID 先就绪。
		folders = convergeFolders(folders)

		for _, f := range folders {
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

	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			for _, t := range timers {
				t.Stop()
			}
			mu.Unlock()
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

// convergeFolders 对本轮待处理目录排序。监控块已改为「每目录只处理自身直接子项」
// （syncDir 非递归），因此不再丢弃子孙目录——每个目录都独立、由各自事件触发。
//
// 仅按路径升序排序，保证父目录先于子目录处理，使子目录的 AddCloudFolder 能拿到
// 已经建好的父 FID。原「丢弃祖先在列表中的子孙」逻辑已不适用（非递归后子孙须独立处理），
// 故此处只排序、保留全部目录。
func convergeFolders(folders []string) []string {
	sort.Strings(folders) // 升序：父目录必然先于子目录，保证父 FID 先就绪
	return folders
}
