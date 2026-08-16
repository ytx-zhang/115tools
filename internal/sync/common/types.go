package common

import (
	"context"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
)

// Entry 是 VisitFile 回调收到的文件元数据。
type Entry struct {
	IsVideo  bool
	Size     int64
	PickCode string
}

// Visitor 定义云端遍历回调。Walker.Walk 负责递归/并发/配额/分页/取消，
// 使用方通过回调决定「拿到目录/文件后做什么」。
type Visitor struct {
	EnterDir  func(ctx context.Context, path, fid string) (descend bool, err error)
	VisitFile func(ctx context.Context, path, fid, pickCode string, e Entry) error
	// SkipByCount：云端总数与 DB 记录数一致则跳过该目录（大库二次同步提速）。
	SkipByCount bool
}

// Paths 是本/云端目录路径与文件系统交互参数（由 runner 从配置组装一次，指针共享）。
// ⚠️ 共享指针是关键：SyncFid/TempFid/StrmFid 在 Init 时才从云端解析写入，
// 所有子包必须持同一指针才能看到 Init 之后写入的 FID（否则值拷贝永远为空）。
type Paths struct {
	SyncPath string // 本地媒体文件同步根
	SyncFid  string // 云端同步根 FID（运行时从 DB 获取）
	TempPath string // 云端临时回收目录（本地无目录）
	TempFid  string // 回收目录 FID（运行时从 DB 获取）
	StrmPath string // strm 链接本地输出目录
	StrmFid  string // strm 目录对应云端 FID（Init 时从 GetDirInfo 获得，运行时直接复用）
	StrmUrl  string // strm 链接前缀（http://...）
	Debounce time.Duration
}

// NewPaths 从配置装配路径对象（含去抖窗口默认值归一），返回指针供各子包共享。
func NewPaths(cfg *config.Config) *Paths {
	debounce := time.Duration(cfg.DebounceMinutes) * time.Minute
	if cfg.DebounceMinutes <= 0 {
		debounce = 10 * time.Minute // 配置未设防抖时的兜底默认
	}
	return &Paths{
		SyncPath: cfg.SyncPath,
		TempPath: cfg.TempPath,
		StrmPath: cfg.StrmPath,
		StrmUrl:  cfg.StrmUrl,
		Debounce: debounce,
	}
}
