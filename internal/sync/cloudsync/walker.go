// Package cloudsync 提供「云端 → 本地」同步任务及其基础能力（walker 遍历 + strmIO 落地）。
// 被 localsync（uploader 写 strm）与 strmgen 复用，故 StrmIO/Walker 在此导出。
package cloudsync

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/common"
	"golang.org/x/sync/errgroup"
)

// walkSem 限制目录递归协程并发（历史教训固化的并发上限）。
const walkSem = 64

// Walker 云端递归遍历小模块。
// 依赖：api（拉取云端列表）、db（计数跳过比对）、rules（isv 兜底判视频）。
// 被调用方：cloudsync/strmgen 任务与顶层 Init（建索引）。
// 产出：按 Visitor 回调把「目录/文件」分派给使用方，不承载具体业务。
type Walker struct {
	api   *drive.Client
	db    *store.Store
	rules common.Rules
}

// NewWalker 构造 walker 小模块（依赖注入）。
func NewWalker(deps *common.SyncDeps) *Walker {
	return &Walker{api: deps.API, db: deps.DB, rules: deps.Rules}
}

// Walk 递归遍历云端目录树。流程：计数跳过（可选）→ 拉取子项 →
// 目录交 EnterDir（递归受 walkSem 信号量限制 64 协程）→ 文件交 VisitFile。
// GetFileList 致命失败调 onFatal 并返回错误（errgroup 汇总首错并取消 gctx）；文件级错误不传播。
//
// ⚠️ 并发模型（勿改成 errgroup.SetLimit）：
//   - 信号量在 walk 入口获取、在「派发完子协程后」主动释放。
//   - errgroup 只负责：子协程生命周期管理 + 首错汇总 + 取消传播（gctx）。
func (w *Walker) Walk(ctx context.Context, rootPath, rootFid string, v common.Visitor, onFatal func(error)) error {
	dirSem := make(chan struct{}, walkSem)
	g, gctx := errgroup.WithContext(ctx)

	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		logs.Debug(logs.ModuleSync, "进入目录", "路径", path)

		select {
		case dirSem <- struct{}{}:
		case <-gctx.Done():
			return gctx.Err()
		}
		done := false
		defer func() {
			if !done {
				<-dirSem
			}
		}()

		if v.SkipByCount {
			info, err := w.api.GetDirInfo(gctx, path)
			if err != nil {
				logs.Warn(logs.ModuleSync, "GetDirInfo 失败，回退全量同步", "路径", path, "错误", err)
			} else {
				cloudTotal := int64(info.FileCount) + int64(info.FolderCount)
				dbTotal := w.db.CountRecursive(path)
				if dbTotal > 0 && cloudTotal == dbTotal {
					return nil
				}
			}
		}

		items, err := w.api.GetFileList(gctx, fid, path)
		if err != nil {
			if onFatal != nil {
				onFatal(fmt.Errorf("获取列表失败[%s]: %w", path, err))
			}
			return err
		}

		for _, item := range items {
			if err := gctx.Err(); err != nil {
				return err
			}
			fullPath := filepath.Join(path, item.Name)

			if item.IsDir {
				descend := true
				if v.EnterDir != nil {
					d, derr := v.EnterDir(gctx, fullPath, item.Fid)
					if derr != nil {
						logs.Error(logs.ModuleSync, "目录处理失败", "路径", fullPath, "错误", derr)
					} else {
						descend = d
					}
				}
				if descend {
					g.Go(func() error { return walk(fullPath, item.Fid) })
				}
				continue
			}

			if v.VisitFile != nil {
				// ⚠️ isv 兜底：115 的 isv 字段经常缺失，本地扩展名命中即视为视频。
				isVideo := item.IsVideo || w.rules.IsVideoExt(item.Name)
				if ferr := v.VisitFile(gctx, fullPath, item.Fid, item.PickCode,
					common.Entry{IsVideo: isVideo, Size: item.Size, PickCode: item.PickCode}); ferr != nil {
					logs.Error(logs.ModuleSync, "文件处理失败", "路径", fullPath, "错误", ferr)
				}
			}
		}

		<-dirSem
		done = true
		return nil
	}

	rootErr := walk(rootPath, rootFid)
	gErr := g.Wait()
	if rootErr != nil {
		return rootErr
	}
	return gErr
}
