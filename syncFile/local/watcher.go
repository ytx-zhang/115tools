package local

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/sgtdi/fswatcher"
)

// watch 是文件监听器的主循环（常驻协程，ctx 取消时退出）。
//
// 职责只有一个：把文件系统的原始事件翻译成「路径变更」，经 Enqueue 交给队列。
// 不做任何业务判定——判定全部在 processPath 中以「本地现状 + 数据库记录」为准，
// 因此事件重复、迟到、乱序都是安全的（幂等）。
func (l *Local) watch(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(l.env.Paths.SyncPath),
		fswatcher.WithCooldown(3*time.Second), // 事件去抖：3 秒内的同路径事件合并
		fswatcher.WithBufferSize(40960),       // 事件缓冲，防止高峰期丢失
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", l.env.Paths.SyncPath)

	for {
		select {
		case <-ctx.Done():
			slog.Info("文件监听器已退出")
			return
		case event, ok := <-watcher.Events():
			if !ok {
				return
			}
			// 事件队列溢出说明丢了部分事件，无法得知具体是哪些路径，
			// 只能触发一次主目录全量扫描兜底，保证不丢变更。
			if slices.Contains(event.Types, fswatcher.EventOverflow) {
				slog.Warn("文件监听事件队列溢出，触发全量扫描兜底", "路径", l.env.Paths.SyncPath)
				l.Enqueue(l.env.Paths.SyncPath)
			}
			if len(event.Types) == 0 {
				continue
			}
			// 入队前收敛：文件直接上报；目录按「是否有子项」决定是否忽略，
			// 避免同一棵子树被多层目录事件重复递归扫描。
			l.enqueueFromWatch(event.Path)
		}
	}
}

// enqueueFromWatch 是 watcher 上报事件前的入口收敛，规则如下：
//
//   - 文件事件：直接 Enqueue，由 processPath 精确判定动作；
//   - 目录事件：
//     1. 主同步根目录 SyncPath 永远 Enqueue——它是整树递归的唯一总入口；
//     2. 空目录 Enqueue——运行中新增的空目录没有文件事件能触发回退链，
//     必须自己建云端目录，否则该目录在云端永远漏建；
//     3. 有子项的目录直接忽略：其子文件/子目录会单独上报并触发 processPath
//     内部的回退链（「父目录未同步则 Enqueue 父目录」），最终由「父目录
//     已在云端」的那一层递归 syncDir 把整棵子树收敛处理。
//
// 为什么忽略有子项的目录能去重：fswatcher 对一棵新子树，最深的叶子最先报、
// 祖先最后报，所以中间层目录事件到达时其下已存在子项；再让它进 processPath
// 跑一次 syncDir，会和根目录的递归形成「同一棵子树被多层目录重复递归」的双入口，
// 正是之前重复上传队列的根源。忽略中间层目录事件后，单入口递归即可覆盖全树。
//
// 注意：回退链（processPath 内 Enqueue 父目录）不走此过滤，它是云端目录树
// 建立责任的兜底承担者，必须保留。
func (l *Local) enqueueFromWatch(path string) {
	info, err := os.Stat(path)
	if err != nil {
		// 路径已不存在（删除/重命名瞬间）：交给 processPath 判定，通常为无需动作
		l.Enqueue(path)
		return
	}
	if !info.IsDir() {
		// 文件：直接上报
		l.Enqueue(path)
		return
	}
	// 目录：根目录永远处理（整树递归总入口）
	if filepath.Clean(path) == filepath.Clean(l.env.Paths.SyncPath) {
		l.Enqueue(path)
		return
	}
	entries, err := os.ReadDir(path)
	if err != nil || len(entries) == 0 {
		// 读失败或空目录：保守处理；空目录需自己建云端目录，否则漏建
		l.Enqueue(path)
		return
	}
	// 有子项：忽略，交由回退链/父目录递归处理，避免双入口重复扫描
	slog.Debug("忽略有子项的目录事件", "目录", path)
}
