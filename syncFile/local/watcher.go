package local

import (
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
// 「云端已存在」的父目录跑 syncDir 递归。syncDir 递归会补全缺失子孙，故
// 云端不存在的父目录直接跳过也丢不了数据。
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

	// processReady 取出所有已静默目录，逐个对「云端已存在」的父目录跑 syncDir。
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

		// 收敛：只在本轮批处理跑一次，避免递归 syncDir 对同一棵子树重复扫描。
		folders = convergeFolders(folders)

		for _, f := range folders {
			fid := l.env.DB.GetFid(f)
			if fid == "" {
				continue // 父目录云端不存在 → 跳过，祖先事件会覆盖
			}
			l.syncDir(ctx, f, fid, nil)
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

// convergeFolders 丢弃「祖先也在列表」的文件夹，避免递归 syncDir 对同一棵子树重复扫描。
//
// 云端与 DB 均为树结构，子孙不可能脱离祖先独立存在——某文件夹的祖先若也在列表里，
// 祖先的递归 syncDir 必然覆盖它；即便祖先因云端 fid 为空被跳过，子孙 fid 也必为空、
// 本就会被跳过，故丢弃无害。
//
// 先按路径升序排序，祖先必然先于子孙出现；于是向上爬时只需在「已保留集合」(keptSet)
// 里找祖先——被丢弃文件夹的最深祖先必是保留项（自身无祖先在列表里），爬到的第一个命中
// 即其最深保留祖先，绝不会漏判；未命中则 f 自身是新的最上层，纳入 keptSet。
func convergeFolders(folders []string) []string {
	sort.Strings(folders)
	kept := folders[:0]
	keptSet := make(map[string]struct{}, len(folders))
	for _, f := range folders {
		redundant := false
		// 从 f 的父目录开始向上爬，命中已保留集合即判冗余丢弃。
		// 注意：filepath.Dir(根目录) 返回根自身（如 Dir("/")=="/"、Dir("Z:\\")=="Z:\\"），
		// 爬到根后 a 不再变化，若以「a != f」作终止条件会恒真 → 无限循环（watchPump 卡死、
		// 增量同步永不触发）。故必须显式判断「已爬到根」才停止。
		for a := filepath.Dir(f); ; a = filepath.Dir(a) {
			if _, ok := keptSet[a]; ok {
				redundant = true
				break
			}
			if parent := filepath.Dir(a); parent == a {
				break // 已到根目录，无法再向上，停止爬升
			}
		}
		if !redundant {
			kept = append(kept, f)
			keptSet[f] = struct{}{}
		}
	}
	return kept
}
