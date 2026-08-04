package sync

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
)

// Entry 是 VisitFile 回调收到的文件元数据。
type Entry struct {
	IsVideo  bool
	Size     int64
	PickCode string
}

// Visitor 定义云端遍历回调。WalkCloud 负责递归/并发/配额/分页/取消，
// 使用方通过回调决定「拿到目录/文件后做什么」。
type Visitor struct {
	EnterDir  func(ctx context.Context, path, fid string) (descend bool, err error)
	VisitFile func(ctx context.Context, path, fid, pickCode string, e Entry) error
	// SkipByCount：云端总数与 DB 记录数一致则跳过该目录（大库二次同步提速）。
	SkipByCount bool
}

// WalkCloud 递归遍历云端目录树。流程：计数跳过（可选）→ 拉取子项 →
// 目录交 EnterDir（递归受 dirSem 限制 64 协程）→ 文件交 VisitFile。
// GetFileList 致命失败调 onFatal 并返回错误；文件级错误不向上传播。
// API 并发由 drive 的 resty 限流（3/s + burst 5）兜底，不再在此持配额。
func (e *Env) WalkCloud(ctx context.Context, rootPath, rootFid string, v Visitor, onFatal func(error)) error {
	dirSem := make(chan struct{}, 64) // 限制目录递归协程并发

	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		slog.Debug("[云端遍历] 进入目录", "路径", path)

		// ⚠️ 信号量在 walk 入口获取、在 wg.Wait 前主动释放。
		// 父协程不代为子协程获取信号量（避免「64 个活跃协程全部
		// 卡在 sem<- 等待孙协程」的死锁）。
		select {
		case dirSem <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		done := false
		defer func() {
			if !done {
				<-dirSem
			}
			// wg 在信号量释放后 Wait：子协程有机会拿到槽位、不被饿死。
		}()

		// 第一步：计数跳过优化。GetDirInfo 是一次很轻的 API 调用，
		// 而 DB 前缀扫描是毫秒级的本地操作，两者比对一致即可跳过整目录。
		if v.SkipByCount {
			info, err := e.API.GetDirInfo(ctx, path)
			if err != nil {
				slog.Warn("[云端遍历] GetDirInfo 失败，回退全量同步", "路径", path, "错误", err)
			} else {
				cloudTotal := info.FileCount + info.FolderCount
				dbTotal := e.DB.CountRecursive(path)
				slog.Debug("[云端遍历] 计数比对", "路径", path, "云端", cloudTotal, "本地", dbTotal)
				if dbTotal > 0 && cloudTotal == dbTotal {
					slog.Debug("[云端遍历] 跳过未变化目录", "路径", path, "子项数", cloudTotal)
					return nil
				}
			}
		}

		// 第二步：拉文件列表。API 并发由 drive 的 resty 限流（3/s + burst 5）兜底。
		slog.Debug("[云端遍历] 获取文件列表", "路径", path)
		items, err := e.API.GetFileList(ctx, fid)
		if err != nil {
			if onFatal != nil {
				onFatal(fmt.Errorf("[云端遍历] 获取列表失败[%s]: %w", path, err))
			}
			return err
		}
		slog.Info("[云端遍历] 获取文件列表完成", "路径", path, "条目数", len(items))

		// 第三步：逐项分派给回调。wg 等待本目录派生的子目录协程全部结束。
		var wg sync.WaitGroup

		for _, item := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			fullPath := filepath.Join(path, item.Name)

			if item.IsDir {
				descend := true
				if v.EnterDir != nil {
					d, derr := v.EnterDir(ctx, fullPath, item.Fid)
					if derr != nil {
						slog.Error("[云端遍历] 目录处理失败", "路径", fullPath, "错误", derr)
					} else {
						descend = d
					}
				}
				if descend {
					// 子协程自行获取信号量（而非父协程代领），避免死锁。
					wg.Go(func() {
						_ = walk(fullPath, item.Fid)
					})
				}
				continue
			}

			if v.VisitFile != nil {
				if ferr := v.VisitFile(ctx, fullPath, item.Fid, item.PickCode,
					Entry{IsVideo: item.IsVideo, Size: item.Size, PickCode: item.PickCode}); ferr != nil {
					slog.Error("[云端遍历] 文件处理失败", "路径", fullPath, "错误", ferr)
				}
			}
		}

		// 主动释放信号量再等子协程——释放后子协程可以拿到槽位继续工作，
		// 避免了旧版「父协程持槽等子协程 → 子协程等槽 → 死锁」。
		<-dirSem
		done = true

		wg.Wait()
		// ⚠️ 子协程错误被 `_ = walk(...)` 丢弃，但 onFatal 已通过
		// WithCancelCause 把原始错误存入 ctx，这里捞出向上传播。
		if err := context.Cause(ctx); err != nil {
			return err
		}
		return nil
	}

	return walk(rootPath, rootFid)
}
