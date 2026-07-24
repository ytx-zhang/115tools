package local

import (
	"context"
	"log/slog"
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
			// 直接上报具体文件/目录路径，由 processPath 精确判定动作，
			// 不上卷到父目录（避免一次改名污染顶层目录、触发全盘扫描）。
			l.Enqueue(event.Path)
		}
	}
}
