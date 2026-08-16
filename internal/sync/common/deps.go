package common

import (
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/store"
)

// SyncDeps 本地/云端同步子模块共享的 4 个基础依赖。
// NewScanner / NewUploader / NewCloudOps / cloudsync.NewWalker 都重复同一组参数，
// 抽成统一载体避免每个构造函数一长串重复形参、调用点也更清爽。
// 放本包是因为它被 localsync/cloudsync 两个子包共用（若放 sync 根或任一子包会导致
// 被另一方反向依赖 → 循环 import），common 是它们共同依赖的底层包，天然无环。
type SyncDeps struct {
	API   *drive.Client
	DB    *store.Store
	Paths *Paths
	Rules Rules
}
