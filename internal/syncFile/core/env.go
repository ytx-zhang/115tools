// Package core 是 syncFile 三个模块（local/cloud/strm）的共享层：
// Env（运行环境）、WalkCloud（云端遍历器）、TaskStats（进度统计）、文件存取工具。
// 本包不依赖任何功能模块（依赖方向：模块 → core）。
package core

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"time"
)

// Env 是三个模块共享的运行环境，由 NewEnv 构造一次后以指针注入各模块。
type Env struct {
	API   *drive.Open115
	DB    *db.DB
	Paths *Paths
	Sem   chan struct{} // API 并发配额（容量 5），仅 GetFileList 期间持有

	CronEnabled  bool
	CronInterval time.Duration
}

func NewEnv(cfg *config.Config, api *drive.Open115, boltDB *db.DB) *Env {
	VideoExts = cfg.VideoExts
	SetUploadExclude(cfg.UploadExclude)
	return &Env{
		API: api, DB: boltDB,
		Paths: &Paths{
			SyncPath: cfg.SyncPath, TempPath: cfg.TempPath,
			StrmPath: cfg.StrmPath, StrmUrl: cfg.StrmUrl,
			Debounce: DebounceDuration(cfg.DebounceSeconds),
		},
		Sem:          make(chan struct{}, 5),
		CronEnabled:  cfg.CronEnabled(),
		CronInterval: cfg.CronInterval(),
	}
}

// AcquireSlot 获取 API 并发配额。⚠️ 必须在 API 调用后立即释放，
// 不得持有到子目录递归结束（会死锁，见 WalkCloud 用法）。
func (e *Env) AcquireSlot(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case e.Sem <- struct{}{}:
		return true
	}
}
