package app

import "github.com/ytx-zhang/115tools/internal/logs"

// ──── 日志中心代理 ────
// web 层只经 App 订阅日志流（SSE）；Hub 的实现细节不对外暴露。

// Subscribe 订阅日志流（SSE 用）。
func (b *App) Subscribe() chan logs.Entry { return b.hub.Subscribe() }

// Unsubscribe 取消日志订阅。
func (b *App) Unsubscribe(ch chan logs.Entry) { b.hub.Unsubscribe(ch) }

// RecentFiltered 返回最近最多 limit 条指定分类的日志（前端类别查询用）。
func (b *App) RecentFiltered(cat string, limit int) []logs.Entry {
	return b.hub.RecentFiltered(logs.LogFilter(cat), limit)
}

// LogCounts 返回各分类当前可见计数（前端 chip 用，基于 ring 扫描，与回放/翻页一致）。
func (b *App) LogCounts() map[string]int64 { return b.hub.Counts() }

// LogHistory 返回某分类中 Seq<before 的最近最多 limit 条日志（升序），供前端向上滚动加载更早历史。
func (b *App) LogHistory(cat string, before, limit int64) []logs.Entry {
	return b.hub.History(logs.LogFilter(cat), before, int(limit))
}

// ClearLogs 清空内存日志缓冲。
func (b *App) ClearLogs() { b.hub.Clear() }
