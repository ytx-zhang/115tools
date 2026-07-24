package local

import (
	"context"
	"log/slog"
	"time"
)

// 本文件实现「按路径 + 静默窗口」的待处理任务队列。
//
// 设计动机：文件监听器上报的事件非常细碎（一次保存可能触发十几个事件），
// 且文件可能仍在被写入。所以事件先进队列「冷静」一个静默窗口（Settle），
// 窗口期内同一文件的新事件只会刷新计时，直到文件真正消停了才处理。

// Enqueue 将发生变化的路径登记到待处理队列，并记录当前时间用于静默窗口判定。
// 同一路径若再次收到事件，时间被刷新 → 静默窗口重新计时，自然吸收连续写入。
//
// 调用方：文件监听器（watcher.go）、processPath 的「父目录未同步」回退、
// 定时全量同步（cron 经根包调用）、Start 的首次全量登记。
func (l *Local) Enqueue(path string) {
	if path == "" {
		return
	}
	l.mu.Lock()
	l.paths[path] = time.Now()
	l.mu.Unlock()

	// 唤醒队列 worker。通道缓冲为 1：worker 醒着时无需重复投递，
	// 反正它醒来后会一次性处理所有到期任务。
	select {
	case l.ch <- struct{}{}:
	default:
	}
}

// getDueTasks 取出已静默满一个窗口期的任务；期间又来新事件的路径会被自然推迟
// （因为时间戳被刷新，now.Sub(ts) 还不足一个窗口）。
func (l *Local) getDueTasks(now time.Time, settle time.Duration) []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var due []string
	for path, ts := range l.paths {
		if now.Sub(ts) >= settle {
			due = append(due, path)
			delete(l.paths, path)
		}
	}
	return due
}

// earliestWait 返回距离下一个任务到期还需等待的时间；队列无任务时返回 -1。
func (l *Local) earliestWait(settle time.Duration) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.paths) == 0 {
		return -1
	}
	now := time.Now()
	// 直接遍历取最小等待，避免每轮分配切片。
	var minWait time.Duration
	first := true
	for _, ts := range l.paths {
		w := max(settle-now.Sub(ts), 0)
		if first || w < minWait {
			minWait = w
			first = false
		}
	}
	return minWait
}

// queueWorker 是队列的消费主循环（常驻协程，ctx 取消时退出）。
//
// 工作节奏：
//  1. 阻塞等待「有新任务」信号；
//  2. 醒来后持续排空：取出全部到期任务逐条 processPath；
//  3. 没有到期任务但队列非空 → 睡到最近一个任务到期（或新信号唤醒）；
//  4. 队列彻底空了 → 回到第 1 步继续等信号。
func (l *Local) queueWorker(ctx context.Context) {
	settle := l.env.Paths.Settle
	for {
		// 等待新任务信号或上下文取消
		select {
		case <-ctx.Done():
			slog.Info("本地文件同步Worker已退出")
			return
		case <-l.ch:
		}

		// 排空：持续处理已到期的任务
		for {
			if err := ctx.Err(); err != nil {
				return
			}
			tasks := l.getDueTasks(time.Now(), settle)
			if len(tasks) == 0 {
				// 无到期任务：若仍有未到期任务，等待其最早到期后再重试
				if wait := l.earliestWait(settle); wait < 0 {
					break // 队列已空，回到外层等待信号
				} else {
					select {
					case <-ctx.Done():
						return
					case <-time.After(wait):
						continue
					case <-l.ch:
						continue
					}
				}
			}
			for _, path := range tasks {
				if err := ctx.Err(); err != nil {
					return
				}
				l.processPath(ctx, path)
			}
		}
	}
}
