package syncFile

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
)

// CloudEntry 是 WalkCloud 在遍历到文件项时向 VisitFile 回调透传的元数据。
// 目录项不在此结构中（目录由 EnterDir 以 path/fid 形式回调）。
type CloudEntry struct {
	IsVideo  bool   // 是否为视频（需要生成 .strm）
	Size     int64  // 文件大小
	PickCode string // 115 pickcode，用于获取下载直链
}

// CloudVisitor 定义云端遍历的回调。WalkCloud 负责统一的并发、配额、分页与 ctx 检查，
// 具体的目录/文件业务处理下沉到回调，从而消除 cloudScan/cloudSync/runAddStrmTask 的重复骨架。
type CloudVisitor struct {
	// EnterDir 处理目录项，返回是否继续递归其子目录。
	EnterDir func(ctx context.Context, path, fid string) (descend bool, err error)
	// VisitFile 处理文件项。
	VisitFile func(ctx context.Context, path, fid, pickCode string, e CloudEntry) error
	// SkipByCount 启用目录级计数跳过优化：进入每个目录前先调 GetDirInfo 获取云端
	// 递归子项总数，与 db.CountRecursive 比对。匹配则跳过 GetFileList。
	SkipByCount bool
}

// WalkCloud 递归遍历云端目录树，对每项调用对应回调。
//
// 行为约定（与原 cloudScan/cloudSync/runAddStrmTask 一致）：
//   - 每个目录节点获取一个并发配额（acquireSlot）后调用 GetFileList；
//   - 目录项交由 EnterDir 处理，返回 descend=true 时以独立 goroutine 递归；
//   - 文件项交由 VisitFile 处理；
//   - 任一目录的 GetFileList 致命失败时调用 onFatal（可为 nil）并停止该分支。
//
// 返回的错误仅来自 GetFileList 致命失败；文件级错误由各回调内部记录，不向上传播。
func (s *SyncFile) WalkCloud(ctx context.Context, rootPath, rootFid string, v CloudVisitor, onFatal func(error)) error {
	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		slog.Debug("[云端遍历] 进入目录", "路径", path)

		// 计数跳过优化：GetDirInfo（1 次轻量 API）返回递归总数，
		// 与 DB 前缀扫描得到的递归计数比对，一致则跳过 GetFileList。
		if v.SkipByCount {
			info, err := s.api.GetDirInfo(ctx, path)
			if err != nil {
				slog.Debug("[云端遍历] GetDirInfo 失败，回退全量同步", "路径", path, "错误", err)
			} else {
				cloudTotal := info.FileCount + info.FolderCount
				dbTotal := s.db.CountRecursive(path)
				slog.Debug("[云端遍历] 计数比对", "路径", path, "云端", cloudTotal, "本地", dbTotal)
				if dbTotal > 0 && cloudTotal == dbTotal {
					slog.Debug("[云端遍历] 跳过未变化目录", "路径", path, "子项数", cloudTotal)
					return nil
				}
			}
		}

		if !s.acquireSlot(ctx) {
			slog.Debug("[云端遍历] 获取配额失败(已取消)，退出目录", "路径", path)
			return nil
		}
		defer func() { <-s.sem }()

		slog.Debug("[云端遍历] 获取文件列表", "路径", path)
		items, err := s.api.GetFileList(ctx, fid)
		if err != nil {
			if onFatal != nil {
				onFatal(fmt.Errorf("[云端遍历] 获取列表失败[%s]: %w", path, err))
			}
			return err
		}
		slog.Debug("[云端遍历] 获取文件列表完成", "路径", path, "条目数", len(items))

		var wg sync.WaitGroup
		defer wg.Wait()

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
					wg.Go(func() {
						_ = walk(fullPath, item.Fid)
					})
				}
				continue
			}
			if v.VisitFile != nil {
				if ferr := v.VisitFile(ctx, fullPath, item.Fid, item.PickCode,
					CloudEntry{IsVideo: item.IsVideo, Size: item.Size, PickCode: item.PickCode}); ferr != nil {
					slog.Error("[云端遍历] 文件处理失败", "路径", fullPath, "错误", ferr)
				}
			}
		}
		return nil
	}

	return walk(rootPath, rootFid)
}
