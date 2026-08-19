package sync

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// runInit 完成运行时初始化：创建目录、查询/创建云端目录、写入 DB FID 记录，
// 解析 SyncFid/TempFid/StrmFid，最后构建云端索引（WalkCloud 建库）。
// 返回 walked 指示是否执行了 WalkCloud 全量建索引。
func (r *Runner) runInit(ctx context.Context) (walked bool, err error) {
	var rebuildIndex bool // FID 变更时标记为 true，仅影响日志措辞
	type dirInit struct {
		path    string
		local   bool
		fidDest *string // 解析到的 FID 写入此处
	}
	dirs := []dirInit{
		{r.paths.SyncPath, true, &r.paths.SyncFid},
		{r.paths.StrmPath, true, &r.paths.StrmFid},
		{r.paths.TempPath, false, &r.paths.TempFid},
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
		info, err := r.api.GetDirInfo(ctx, d.path)
		if err != nil {
			return false, fmt.Errorf("[初始化] 查询云端目录失败 %s: %w", d.path, err)
		}
		dbFid := r.db.GetFid(d.path)
		if dbFid != "" && dbFid != info.Fid {
			logs.Info(logs.ModuleSync, "云端目录FID变更，清空数据库记录", "路径", d.path)
			r.db.BatchClearPaths([]string{d.path})
			if d.path == r.paths.SyncPath {
				rebuildIndex = true
			}
		}
		if dbFid != info.Fid {
			r.db.SaveRecord(d.path, info.Fid, store.SizeDir)
		}
		if d.fidDest != nil {
			*d.fidDest = info.Fid
		}
		if _, err := r.co.AddCloudFolder(ctx, d.path); err != nil {
			return false, fmt.Errorf("[初始化] 创建云端目录失败 %s: %w", d.path, err)
		}
	}

	if r.db.CountRecursive(r.paths.SyncPath) > 0 {
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
			logs.Error(logs.ModuleSync, "云端扫描被中止，正在清理数据库", "错误", scanErr)
			r.db.BatchClearPaths([]string{r.paths.SyncPath})
		}
	}()

	scanErr = r.wk.Walk(walkCtx, r.paths.SyncPath, r.paths.SyncFid, common.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			r.db.SaveRecord(path, fid, store.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, en common.Entry) error {
			saveSize := en.Size
			if en.IsVideo {
				path, saveSize = common.VideoStrmMeta(path)
				if info, err := os.Stat(path); err == nil {
					// 本地已存在对应 strm（迁移自其他项目的旧 strm），按云端 fid 校验归属后
					// 规范化链接地址（旧 strm 可能指向其他 host），并取写盘后实际 mtime 记 DB。
					if matched, rewrote, mt := common.NormalizeOwnedStrm(r.paths.StrmUrl, path, fid, info.ModTime().Unix()); matched {
						if rewrote {
							logs.Info(logs.ModuleSync, "规范化旧STRM链接", "路径", path)
						}
						saveSize = mt
					}
				}
			}
			r.db.SaveRecord(path, fid, saveSize)
			return nil
		},
	}, func(err error) { walkCancel(err) })
	if scanErr != nil {
		walkCancel(scanErr)
		return true, scanErr
	}
	logs.Info(logs.ModuleSync, "云端数据库索引构建完成", "路径", r.paths.SyncPath, "耗时", time.Since(walkStart).String())
	return true, nil
}
