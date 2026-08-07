package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// ──── 运行配置 ────

// Env 运行时配置，由 Syncer.Initialize() 注入。
type Env struct {
	API          *drive.Open115
	DB           *db.DB
	Paths        Paths
	CronEnabled  bool
	CronInterval time.Duration
}

// Paths 本/云端目录路径与 FID 及文件系统交互参数。
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

// NewEnv 从 config.Config 装配运行时配置并设置全局过滤规则。
func NewEnv(api *drive.Open115, database *db.DB, cfg *config.Config) *Env {
	cronEnabled := true
	if cfg.Cron.Enabled != nil {
		cronEnabled = *cfg.Cron.Enabled
	}
	cronInterval := time.Duration(cfg.Cron.IntervalHours) * time.Hour
	if cfg.Cron.IntervalHours == 0 {
		cronInterval = 12 * time.Hour
	}
	debounce := time.Duration(cfg.DebounceSeconds) * time.Second
	if cfg.DebounceSeconds <= 0 || cfg.DebounceSeconds > 10 {
		debounce = DebounceDuration
	}

	env := &Env{
		API: api,
		DB:  database,
		Paths: Paths{
			SyncPath: cfg.SyncPath,
			TempPath: cfg.TempPath,
			StrmPath: cfg.StrmPath,
			StrmUrl:  cfg.StrmUrl,
			Debounce: debounce,
		},
		CronEnabled:  cronEnabled,
		CronInterval: cronInterval,
	}
	SetVideoExts(cfg.VideoExts)
	SetUploadExclude(cfg.UploadExclude)
	return env
}

// Init 完成 Env 运行时初始化：创建目录、查询/创建云端目录、
// 写入 DB FID 记录并解析 SyncFid/TempFid，最后构建云端索引。
// 返回 walked 指示是否执行了 WalkCloud 全量建索引。
func (e *Env) Init(ctx context.Context) (walked bool, err error) {
	var rebuildIndex bool // FID 变更时标记为 true，仅影响日志措辞
	type dirInit struct {
		path    string
		local   bool
		fidDest *string // 解析到的 FID 写入此处
	}
	dirs := []dirInit{
		{e.Paths.SyncPath, true, &e.Paths.SyncFid},
		{e.Paths.StrmPath, true, &e.Paths.StrmFid},
		{e.Paths.TempPath, false, &e.Paths.TempFid},
	}
	for _, d := range dirs {
		if strings.TrimSpace(d.path) == "" {
			continue
		}
		if d.local {
			if err := os.MkdirAll(d.path, 0755); err != nil {
				return false, fmt.Errorf("[初始化] 创建本地目录失败 %s: %w", d.path, err)
			}
		}
		info, err := e.API.GetDirInfo(ctx, d.path)
		if err != nil {
			return false, fmt.Errorf("[初始化] 查询云端目录失败 %s: %w", d.path, err)
		}
		dbFid := e.DB.GetFid(d.path)
		if dbFid != "" && dbFid != info.Fid {
			logs.Info(logs.ModuleSync, "云端目录FID变更，清空数据库记录", "路径", d.path)
			e.DB.BatchClearPaths([]string{d.path})
			if d.path == e.Paths.SyncPath {
				rebuildIndex = true
			}
		}
		// GetDirInfo 已确认云端目录存在，将 FID 写入 DB 避免 AddCloudFolder 重复创建
		if dbFid != info.Fid {
			e.DB.SaveRecord(d.path, info.Fid, db.SizeDir)
		}
		if d.fidDest != nil {
			*d.fidDest = info.Fid
		}
		if _, err := AddCloudFolder(ctx, e, d.path); err != nil {
			return false, fmt.Errorf("[初始化] 创建云端目录失败 %s: %w", d.path, err)
		}
	}

	// DB 已有索引则跳过 WalkCloud（路径变更/FID 变更后 Broker 或本方法已清空记录，自然触发重建）
	if e.DB.CountRecursive(e.Paths.SyncPath) > 0 {
		return false, nil
	}
	if rebuildIndex {
		logs.Info(logs.ModuleSync, "云端目录FID变更，开始重建数据库索引...")
	} else {
		logs.Info(logs.ModuleSync, "初次运行，开始构建云端数据库索引...")
	}
	if err = context.Cause(ctx); err != nil {
		return false, err
	}
	walkCtx, walkCancel := context.WithCancelCause(ctx)
	defer walkCancel(nil)

	walkStart := time.Now()
	var scanErr error
	defer func() {
		if scanErr != nil {
			logs.Error(logs.ModuleSync, "云端扫描被中止，正在清理数据库", "错误信息", scanErr)
			e.DB.BatchClearPaths([]string{e.Paths.SyncPath})
		}
	}()

	scanErr = e.WalkCloud(walkCtx, e.Paths.SyncPath, e.Paths.SyncFid, Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			e.DB.SaveRecord(path, fid, db.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, en Entry) error {
			saveSize := en.Size
			if en.IsVideo {
				path = strings.TrimSuffix(path, filepath.Ext(path)) + ".strm"
				saveSize = 0
				if info, err := os.Stat(path); err == nil {
					if _, localFid := ExtractPickcode(path); localFid == fid {
						saveSize = info.ModTime().Unix()
					}
				}
			}
			e.DB.SaveRecord(path, fid, saveSize)
			return nil
		},
	}, func(err error) { walkCancel(err) })
	if scanErr != nil {
		walkCancel(scanErr)
		return true, scanErr
	}
	logs.Info(logs.ModuleSync, "云端数据库索引构建完成", "路径", e.Paths.SyncPath, "耗时", time.Since(walkStart).String())
	return true, nil
}
