package kit

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
	"github.com/ytx-zhang/115tools/internal/vault"
	"golang.org/x/sync/errgroup"
)

// Entry 是 VisitFile 回调收到的文件元数据。
type Entry struct {
	IsVideo  bool
	Size     int64
	PickCode string
}

// Visitor 定义云端遍历回调。Walk 负责递归/并发/取消，使用方通过回调决定动作。
type Visitor struct {
	EnterDir  func(ctx context.Context, path, fid string) (descend bool, err error)
	VisitFile func(ctx context.Context, path, fid, pickCode string, e Entry) error
	// SkipByCount：云端总数与索引记录数一致则跳过该目录（大库二次同步提速）。
	SkipByCount bool
}

// Walker 云端递归遍历。依赖：pan（列表）、vault（计数跳过）、rules（isv 兜底判视频）。
type Walker struct {
	api   *pan.Client
	vault *vault.Index
	rules Rules
}

// NewWalker 构造 walker。
func NewWalker(deps *Deps) *Walker {
	return &Walker{api: deps.Pan, vault: deps.Vault, rules: deps.Rules}
}

// Walk 递归遍历云端目录树：计数跳过（可选）→ 拉子项 → 目录交 EnterDir → 文件交 VisitFile。
func (w *Walker) Walk(ctx context.Context, rootPath, rootFid string, v Visitor, onFatal func(error)) error {
	g, gctx := errgroup.WithContext(ctx)

	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		journal.Debug(gctx, "进入云端目录", "路径", path)
		if err := context.Cause(gctx); err != nil {
			return err
		}

		if v.SkipByCount {
			if info, err := w.api.GetDirInfo(gctx, path); err != nil {
				journal.Warn(gctx, "GetDirInfo 失败，回退全量同步", "路径", path, "错误", err)
			} else {
				cloudTotal := int64(info.FileCount) + int64(info.FolderCount)
				dbTotal := w.vault.CountRecursive(gctx, path)
				if dbTotal > 0 && cloudTotal == dbTotal {
					return nil
				}
			}
		}

		items, err := w.api.GetFileList(gctx, fid)
		if err != nil {
			if onFatal != nil {
				onFatal(fmt.Errorf("获取列表失败[%s]: %w", path, err))
			}
			return err
		}

		for _, item := range items {
			if err := context.Cause(gctx); err != nil {
				return err
			}
			fullPath := filepath.Join(path, item.Name)

			if item.IsDir {
				descend := true
				if v.EnterDir != nil {
					d, derr := v.EnterDir(gctx, fullPath, item.Fid)
					if derr != nil {
						journal.Error(gctx, "目录处理失败", "路径", fullPath, "错误", derr)
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
				isVideo := item.IsVideo || w.rules.IsVideoExt(item.Name)
				if ferr := v.VisitFile(gctx, fullPath, item.Fid, item.PickCode,
					Entry{IsVideo: isVideo, Size: item.Size, PickCode: item.PickCode}); ferr != nil {
					journal.Error(gctx, "文件处理失败", "路径", fullPath, "错误", ferr)
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
