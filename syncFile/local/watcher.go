package local

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"time"

	"github.com/sgtdi/fswatcher"
)

// watchPump 是文件监听器主循环（常驻协程，ctx 取消时退出）。
//
// 极简设计：监听器完全「不懂」业务逻辑，只做一件事——
// 把文件系统原始事件对应的「父目录」收进待处理表，
// 等 debounce 静默窗口内无新事件后，对每个「云端已存在」的父目录跑一遍 syncDir 递归。
// 因为 syncDir 是递归的，只要某个上层祖先在云端存在，它一跑就把下面缺失的全补上；
// 那些「云端还不存在」的父目录直接跳过，反正它们自己（或祖先）迟早也会作为某次
// 事件的父目录进来，被处理时递归创建——所以跳过不会丢数据。
//
// 事件类型完全无视：连 rename/move 也走 syncDir（旧路径判删、新路径判增）。
// 这换取了零类型判断的极简代码，代价是用户改名/整理媒体时旧视频会进 TempFid、
// 新文件重传——已确认接受。
func (l *Local) watchPump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithCooldown(3*time.Second), // 同路径事件去抖：3 秒内的合并推送
		fswatcher.WithBufferSize(40960),       // 事件缓冲，防止高峰期丢失
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", l.env.Paths.SyncPath)

	timer := time.NewTimer(l.env.Paths.Debounce)
	timer.Stop()
	arm := func() {
		// debounce：每次事件都重置窗口，直到静默 Debounce 秒才处理。
		// 先 Stop+排空（标准安全用法），避免已触发但未消费的旧值导致立刻再触发。
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(l.env.Paths.Debounce)
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			// 事件队列溢出说明丢了部分事件、无法得知具体是哪些路径，
			// 只能把主同步目录收进待处理表，靠 syncDir 递归兜底，保证不丢变更。
			if slices.Contains(ev.Types, fswatcher.EventOverflow) {
				slog.Warn("文件监听事件队列溢出，触发主目录全量扫描兜底", "路径", l.env.Paths.SyncPath)
				l.addPending(l.env.Paths.SyncPath)
				l.pendingDirty.Store(true)
				arm()
				continue
			}
			// 任何事件都只记其父目录，无视类型（rename 也走 syncDir）
			l.addPending(ev.Path)
			l.pendingDirty.Store(true)
			arm()
		case <-timer.C:
			l.processPending(ctx)
			if l.pendingDirty.Load() {
				arm() // 处理期间又有新事件 → 再等一轮静默
			}
		}
	}
}

// addPending 把某路径的父目录收进待处理表（文件/目录事件都取其父）。
func (l *Local) addPending(path string) {
	parent := filepath.Dir(path)
	l.pendingMu.Lock()
	l.pending[parent] = struct{}{}
	l.pendingMu.Unlock()
}

// processPending 取出待处理文件夹，逐个对「云端已存在」的父目录跑 syncDir 递归。
//
// 中断语义（用户定稿）：syncDir 每完成一个云端操作会在下一操作前查 pendingDirty；
// 若处理期间来了新事件（pendingDirty 为真），立即把「当前文件夹 + 剩余未处理文件夹」
// 全部加回待处理表并 return——当前正在进行的云端操作（建目录/删文件/投递上传）一旦
// 完成就快速停止，绝不留下「半截 syncDir 又不再续」的不一致状态。
func (l *Local) processPending(ctx context.Context) {
	l.pendingDirty.Store(false)
	l.pendingMu.Lock()
	folders := make([]string, 0, len(l.pending))
	for f := range l.pending {
		folders = append(folders, f)
	}
	l.pending = make(map[string]struct{})
	l.pendingMu.Unlock()

	// 收敛：丢弃「祖先也在本轮列表」的文件夹，避免递归 syncDir 对同一棵子树重复扫描。
	// 云端与 DB 均为树结构，子孙不可能脱离祖先独立存在——某文件夹的祖先若也在列表里，
	// 祖先的递归 syncDir 必然覆盖它；即便祖先因云端 fid 为空被跳过，子孙 fid 也必为空、
	// 本就会被跳过，故丢弃无害。
	folderSet := make(map[string]struct{}, len(folders))
	for _, f := range folders {
		folderSet[f] = struct{}{}
	}
	kept := folders[:0]
	for _, f := range folders {
		redundant := false
		// 从 f 的父目录开始向上爬，若某个祖先也在本轮列表则判冗余丢弃。
		// 注意：filepath.Dir(根目录) 返回根自身（如 Dir("/")=="/"、Dir("Z:\\")=="Z:\\"），
		// 爬到根后 a 不再变化，若以「a != f」作终止条件会恒真 → 无限循环（watchPump 卡死、
		// 增量同步永不触发）。故必须显式判断「已爬到根」才停止。
		for a := filepath.Dir(f); ; a = filepath.Dir(a) {
			if _, ok := folderSet[a]; ok {
				redundant = true
				break
			}
			if parent := filepath.Dir(a); parent == a {
				break // 已到根目录，无法再向上，停止爬升
			}
		}
		if !redundant {
			kept = append(kept, f)
		}
	}
	folders = kept

	for i, f := range folders {
		if l.pendingDirty.Load() {
			// 处理中新事件 → 当前(i)+剩余全回写，快速停止
			l.pendingMu.Lock()
			for _, rf := range folders[i:] {
				l.pending[rf] = struct{}{}
			}
			l.pendingMu.Unlock()
			return
		}
		fid := l.env.DB.GetFid(f)
		if fid == "" {
			continue // 父目录云端不存在 → 跳过，祖先事件会覆盖
		}
		l.syncDir(ctx, f, fid, l.pendingDirty.Load)
	}
}
