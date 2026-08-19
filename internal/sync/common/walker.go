package common

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/store"
	"golang.org/x/sync/errgroup"
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

// Walker 云端递归遍历小模块。
// 依赖：api（拉取云端列表）、db（计数跳过比对）、rules（isv 兜底判视频）。
// 被调用方：cloudsync/strmgen 任务与顶层 Init（建索引）。
// 产出：按 Visitor 回调把「目录/文件」分派给使用方，不承载具体业务。
// 归入 common 是因为它被 cloudsync/strmgen/init 三个子包共用（与 StrmIO 同理，
// 下沉到被各任务子包共享的底层包，可避免循环 import）。
type Walker struct {
	api   *drive.Client
	db    *store.Store
	rules Rules
}

// NewWalker 构造 walker 小模块（依赖注入）。
func NewWalker(deps *Core) *Walker {
	return &Walker{api: deps.API, db: deps.DB, rules: deps.Rules}
}

// Walk 递归遍历云端目录树。流程：计数跳过（可选）→ 拉取子项 →
// 目录交 EnterDir（递归派发子协程）→ 文件交 VisitFile。
// GetFileList 致命失败调 onFatal 并返回错误（errgroup 汇总首错并取消 gctx）；文件级错误不传播。
//
// 并发模型：errgroup 仅负责子协程生命周期管理 + 首错汇总 + 取消传播（gctx），
// 本层不再额外限制目录并发——API 速率已由 drive 层限流兜底，目录再多也只是多派发协程排队。
func (w *Walker) Walk(ctx context.Context, rootPath, rootFid string, v Visitor, onFatal func(error)) error {
	g, gctx := errgroup.WithContext(ctx)

	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		logs.Debug(logs.ModuleSync, "进入目录", "路径", path)

		if err := gctx.Err(); err != nil {
			return err
		}

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
					Entry{IsVideo: isVideo, Size: item.Size, PickCode: item.PickCode}); ferr != nil {
					logs.Error(logs.ModuleSync, "文件处理失败", "路径", fullPath, "错误", ferr)
				}
			}
		}

		return nil
	}

	rootErr := walk(rootPath, rootFid)
	gErr := g.Wait()
	if rootErr != nil {
		return rootErr
	}
	return gErr
}
