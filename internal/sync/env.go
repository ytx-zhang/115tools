package sync

import (
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"time"
)

// Env 是同步各模块共享的运行环境，由 NewEnv 构造一次后以指针注入。
// 不再持有 API 并发配额 Sem——drive 的 resty 客户端已有 3/s + burst 5 全局限流，
// 超额请求在 limiter 排队，效果等价，且消除了「持配额递归死锁」这一注意事项。
type Env struct {
	API   *drive.Open115
	DB    *db.DB
	Paths *Paths

	CronEnabled  bool
	CronInterval time.Duration
}

func NewEnv(cfg *config.Config, api *drive.Open115, boltDB *db.DB) *Env {
	// 注入名单：视频白名单与上传排除名单统一收进本包内的 atomic 变量，
	// 避免原 core.VideoExts 全局变量在「保存配置」与「扫描协程」间的真 race。
	SetVideoExts(cfg.VideoExts)
	SetUploadExclude(cfg.UploadExclude)
	return &Env{
		API: api, DB: boltDB,
		Paths: &Paths{
			SyncPath: cfg.SyncPath, TempPath: cfg.TempPath,
			StrmPath: cfg.StrmPath, StrmUrl: cfg.StrmUrl,
			Debounce: DebounceDuration(cfg.DebounceSeconds),
		},
		CronEnabled:  cfg.CronEnabled(),
		CronInterval: cfg.CronInterval(),
	}
}
