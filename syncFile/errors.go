package syncFile

import (
	"fmt"
	"log/slog"
	"time"
)

// failLog 统一记录任务失败：同时输出结构化日志与写入 Stats.failedErrors，
// 避免 cloudSync/addStrm 中重复的 slog.Error + markFailed 拼装。
func failLog(stats *taskStats, path, action string, err error) {
	slog.Error(action, "文件", path, "错误", err)
	stats.markFailed(fmt.Sprintf("[%s] %s: %s (%v)", time.Now().Format("15:04"), action, path, err))
}
