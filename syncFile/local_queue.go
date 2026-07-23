package syncFile

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/sgtdi/fswatcher"
)

// ──── 任务队列（按路径 + 静默窗口） ────

// addLocalSyncTask 将发生变化的路径登记到待处理队列，并记录当前时间用于 settle 判定。
// 同一路径若再次收到事件，时间被刷新 → 静默窗口重新计时，自然吸收连续写入。
func (s *SyncFile) addLocalSyncTask(path string) {
	if path == "" {
		return
	}
	s.local.mu.Lock()
	s.local.paths[path] = time.Now()
	s.local.mu.Unlock()

	select {
	case s.local.ch <- struct{}{}:
	default:
	}
}

// getDueTasks 取出已静默满 settle 的任务；期间又来新事件的路径会被推迟。
func (s *SyncFile) getDueTasks(now time.Time, settle time.Duration) []string {
	s.local.mu.Lock()
	defer s.local.mu.Unlock()

	var due []string
	for path, ts := range s.local.paths {
		if now.Sub(ts) >= settle {
			due = append(due, path)
			delete(s.local.paths, path)
		}
	}
	return due
}

// earliestWait 返回距离下一个任务到期还需等待的时间；无待处理任务返回 -1。
func (s *SyncFile) earliestWait(settle time.Duration) time.Duration {
	s.local.mu.Lock()
	defer s.local.mu.Unlock()

	if len(s.local.paths) == 0 {
		return -1
	}
	now := time.Now()
	waits := make([]time.Duration, 0, len(s.local.paths))
	for _, ts := range s.local.paths {
		waits = append(waits, max(settle-now.Sub(ts), 0))
	}
	return slices.Min(waits)
}

func (s *SyncFile) startLocalSyncWorker(ctx context.Context) {
	settle := s.paths.Settle
	for {
		// 等待新任务信号或上下文取消
		select {
		case <-ctx.Done():
			slog.Info("本地文件同步Worker已退出")
			return
		case <-s.local.ch:
		}

		// 排空：持续处理已到期的任务
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			tasks := s.getDueTasks(time.Now(), settle)
			if len(tasks) == 0 {
				// 无到期任务：若仍有未到期任务，等待其最早到期后再重试
				if wait := s.earliestWait(settle); wait < 0 {
					break // 队列已空，回到外层等待信号
				} else {
					select {
					case <-ctx.Done():
						return
					case <-time.After(wait):
						continue
					case <-s.local.ch:
						continue
					}
				}
			}
			for _, path := range tasks {
				if err := ctx.Err(); err != nil {
					return
				}
				s.processPath(ctx, path)
			}
		}
	}
}

func (s *SyncFile) watchSyncPath(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(s.paths.SyncPath),
		fswatcher.WithCooldown(3*time.Second),
		fswatcher.WithBufferSize(40960),
	)
	if err != nil {
		slog.Error("监听器启动失败", "err", err)
		return
	}
	go watcher.Watch(ctx)
	slog.Info("文件监听器启动", "路径", s.paths.SyncPath)
	for {
		select {
		case <-ctx.Done():
			slog.Info("文件监听器已退出")
			return
		case event, ok := <-watcher.Events():
			if !ok {
				return
			}
			// inotify 事件队列溢出：丢失了部分事件，触发一次全量扫描兜底
			if slices.Contains(event.Types, fswatcher.EventOverflow) {
				slog.Warn("文件监听事件队列溢出，触发全量扫描兜底", "路径", s.paths.SyncPath)
				s.addLocalSyncTask(s.paths.SyncPath)
			}
			if len(event.Types) == 0 {
				continue
			}
			// 直接上报具体文件/目录路径，由 processPath 按「本地现状 + DB 记录」精确判定动作，
			// 不再上卷到父目录（避免一次改名污染顶层目录、触发全盘扫描）。
			s.addLocalSyncTask(event.Path)
		}
	}
}
