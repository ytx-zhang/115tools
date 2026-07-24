package core

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
)

// Entry 是 WalkCloud 遍历到「文件」时向 VisitFile 回调传递的元数据。
// （目录不走这个结构，目录由 EnterDir 以 path/fid 两个参数单独回调。）
type Entry struct {
	IsVideo  bool   // 是否为视频文件（云端同步/STRM 场景中视频要落地为 .strm 而非下载）
	Size     int64  // 文件大小（字节）
	PickCode string // 115 的 pickcode，换取下载直链的凭证
}

// Visitor 定义云端遍历的回调（访问者模式）：
// WalkCloud 负责所有「体力活」——递归、并发控制、API 配额、分页、取消检查；
// 具体「拿到一个目录/文件后做什么」由使用方通过回调告诉它。
// 三个使用场景：bootstrap 建库扫描、cloud 云端同步、strm STRM 生成。
type Visitor struct {
	// EnterDir 处理目录项，返回 true 表示继续递归它的子目录。
	EnterDir func(ctx context.Context, path, fid string) (descend bool, err error)
	// VisitFile 处理文件项。
	VisitFile func(ctx context.Context, path, fid, pickCode string, e Entry) error
	// SkipByCount 开启「计数跳过」优化：进入每个目录前，先用一次轻量的
	// GetDirInfo 拿到云端递归子项总数，与本地数据库的记录数比对，
	// 一致说明该目录没变化，直接跳过、不再逐页拉取文件列表。
	// 大媒体库二次同步时提速非常明显。仅云端全量同步开启。
	SkipByCount bool
}

// WalkCloud 递归遍历云端目录树，对每一项调用 Visitor 中对应的回调。
//
// 【执行流程】
//  1. （可选）计数跳过：云端总数与 DB 记录数一致 → 整目录跳过；
//  2. 获取 API 并发配额（容量 5）→ 调用 GetFileList 拉取该目录全部子项 → 立即释放配额；
//  3. 遍历子项：目录交给 EnterDir，需要递归时以独立协程继续 walk
//     （协程总数受 dirSem 容量 64 限制，防止超宽目录树把协程数打爆）；
//  4. 文件交给 VisitFile 处理。
//
// 【错误约定】
//   - GetFileList 致命失败：调用 onFatal（可为 nil）并返回错误，停止该分支；
//   - 回调内的文件级错误由使用方自行记录，不向上传播（一个文件失败不拖垮整次遍历）。
func (e *Env) WalkCloud(ctx context.Context, rootPath, rootFid string, v Visitor, onFatal func(error)) error {
	// dirSem 限制目录递归协程的总并发度。
	// 与 Env.Sem（限制 API 并发）是两个独立的闸：一个管协程数量，一个管 API 调用频率。
	dirSem := make(chan struct{}, 64)

	var walk func(path, fid string) error
	walk = func(path, fid string) error {
		slog.Debug("[云端遍历] 进入目录", "路径", path)

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

		// 第二步：拿配额 → 拉文件列表 → 立即还配额。
		if !e.AcquireSlot(ctx) {
			slog.Debug("[云端遍历] 获取配额失败(已取消)，退出目录", "路径", path)
			return nil
		}
		slog.Debug("[云端遍历] 获取文件列表", "路径", path)
		items, err := e.API.GetFileList(ctx, fid)
		// GetFileList 一结束立刻释放配额：配额只约束「同时在飞的 API 调用数」，
		// 不约束目录处理与子目录递归，避免父子目录互相等锁死锁。
		<-e.Sem
		if err != nil {
			if onFatal != nil {
				onFatal(fmt.Errorf("[云端遍历] 获取列表失败[%s]: %w", path, err))
			}
			return err
		}
		slog.Info("[云端遍历] 获取文件列表完成", "路径", path, "条目数", len(items))

		// 第三步：逐项分派给回调。wg 等待本目录派生的子目录协程全部结束。
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
					// 子目录递归前领取目录协程配额；ctx 取消则放弃递归。
					select {
					case dirSem <- struct{}{}:
						wg.Go(func() {
							defer func() { <-dirSem }()
							_ = walk(fullPath, item.Fid)
						})
					case <-ctx.Done():
						return ctx.Err()
					}
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
		return nil
	}

	return walk(rootPath, rootFid)
}
