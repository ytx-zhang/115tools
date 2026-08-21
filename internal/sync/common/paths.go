package common

import (
	"path/filepath"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
)

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
	CacheDir string // 本地缓存根目录（<SyncPath>/.cache）：与源同挂载点以便 cache.Move 走原子 rename
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
		// ⚠️ 缓存必须落在 SyncPath 同挂载点：cache.Move 才能原子 rename；监听/扫描均按此根目录忽略。
		CacheDir: filepath.Join(cfg.SyncPath, ".cache"),
	}
}
