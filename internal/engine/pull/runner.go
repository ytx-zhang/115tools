// Package pull 实现「云端 → 本地」同步：遍历云端树、新文件落地（视频写 strm / 普通下载）、
// 云端冗余删除、顶层项移入回收目录。STRM 生成能力已折叠进本包（ArchiveToTemp 选项即旧收尾动作）。
package pull

import (
	"context"
	"errors"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
	"github.com/ytx-zhang/115tools/internal/pan"
)

// Options 云端同步选项。
type Options struct {
	FetchMissing  bool // 下载云端有、本地无的文件
	DropRedundant bool // 删除云端同名冗余文件（依赖本地索引，仅 push 连带扫描可用）
	GenStrm       bool // 视频落地为 .strm（false = 下载原视频）
	ArchiveToTemp bool // 全部成功后把顶层项移入云端回收目录（等价旧 STRM 生成收尾）
	UseIndex      bool // 是否读写本地路径索引：pull 本体只落本地不落库；push 连带扫描必须落库
}

// Runner 云端→本地同步任务。
type Runner struct {
	api   *pan.Client
	idx   *index.Index
	paths *shared.TaskPaths
	wk    *shared.Walker
	strm  *shared.StrmIO
	opts  Options
}

// NewRunner 构造云端同步任务。
func NewRunner(deps *shared.Deps, wk *shared.Walker, strm *shared.StrmIO, opts Options) *Runner {
	return &Runner{api: deps.Pan, idx: deps.Index, paths: deps.Paths, wk: wk, strm: strm, opts: opts}
}

// Run 执行一轮完整云端同步，统计写入 c。
func (r *Runner) Run(ctx context.Context, c *journal.Counters) error {
	start := time.Now()
	defer func() {
		journal.Info(ctx, "云端同步完成", "路径", r.paths.LocalDir, "耗时", time.Since(start))
	}()
	journal.Info(ctx, "开始云端同步", "路径", r.paths.LocalDir)

	if r.paths.CloudFid == "" {
		return errors.New("云端同步根 FID 未就绪")
	}

	// 仅「归档到回收目录」需要收集顶层 FID，未开启时不做无谓判断
	topFids := make([]string, 0, 8)
	err := r.wk.Walk(ctx, r.paths.CloudDir, r.paths.CloudFid, shared.Visitor{
		// 计数跳过依赖索引：pull 本体不落库，必须逐目录深入（跳过就等于不检查该目录缺什么）；
		// push 的连带扫描有完整索引，才用得上条目数一致跳过。
		SkipByCount: r.opts.UseIndex,
		EnterDir: func(ctx context.Context, path, fid string) (bool, error) {
			localPath := shared.MapCloudToLocal(r.paths.LocalDir, r.paths.CloudDir, path)
			if r.opts.ArchiveToTemp && isTopLevel(r.paths.CloudDir, path) {
				topFids = append(topFids, fid)
			}
			// MkdirAll 幂等，无需先查索引判存在
			if err := os.MkdirAll(localPath, 0o755); err != nil {
				journal.Error(ctx, "创建目录失败", "路径", localPath, "错误", err)
				return false, nil
			}
			if r.opts.UseIndex && r.idx.GetFid(ctx, localPath) == "" {
				r.idx.Put(ctx, localPath, fid, index.SizeDir)
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e shared.Entry) error {
			localPath := shared.MapCloudToLocal(r.paths.LocalDir, r.paths.CloudDir, path)
			genStrm := e.IsVideo && r.opts.GenStrm
			savePath := localPath
			if genStrm {
				savePath = shared.VideoToStrmPath(localPath)
			}

			// 本地是否已存在只看文件系统：索引里有没有记录与本地有没有文件无关
			// （pull 本体不落库，若拿索引判定会把「云端有记录、本地未下载」的文件永久跳过）。
			var dbFid string
			if r.opts.UseIndex {
				dbFid = r.idx.GetFid(ctx, savePath)
			}
			if _, serr := os.Stat(savePath); dbFid != "" || serr == nil {
				if r.opts.DropRedundant && dbFid != "" && dbFid != fid {
					if err := r.api.DeleteFile(ctx, fid, savePath); err != nil {
						journal.Error(ctx, "清理云端冗余项失败", "路径", savePath, "错误", err)
					} else {
						c.Deleted++
					}
				}
				return nil
			}
			if !r.opts.FetchMissing {
				return nil
			}

			c.Scanned++
			size, err := r.strm.FetchAndSave(ctx, pickCode, savePath, genStrm)
			if err != nil {
				c.Failed++
				return nil
			}
			if genStrm {
				c.StrmGenerated++
			} else {
				c.Downloaded++
			}
			if r.opts.UseIndex {
				// 落库供本任务 push 侧比对：否则下次本地扫描会把刚下载的文件当新增重传
				r.idx.Put(ctx, savePath, fid, size)
			}
			return nil
		},
	}, nil)
	if err != nil && !errors.Is(err, context.Canceled) {
		journal.Error(ctx, "云端同步遍历失败", "路径", r.paths.LocalDir, "错误", err)
		return err
	}

	if r.opts.ArchiveToTemp && context.Cause(ctx) == nil && len(topFids) > 0 {
		if err := r.archiveToTemp(ctx, topFids); err != nil {
			journal.Error(ctx, "移动文件至回收目录失败", "错误", err)
			return err
		}
	}
	return nil
}

// archiveToTemp 把顶层 FID 一次性批量移入云端回收目录。
func (r *Runner) archiveToTemp(ctx context.Context, fids []string) error {
	t0 := time.Now()
	if err := r.api.MoveFile(ctx, strings.Join(fids, ","), r.paths.TempFid, r.paths.CloudDir); err != nil {
		return err
	}
	journal.Info(ctx, "移动文件至回收目录", "文件数量", len(fids), "耗时", time.Since(t0))
	return nil
}

// isTopLevel 判断云端路径是否为同步根的直接子项（用于「移入回收目录」收集顶层 FID）。
// 云端路径统一以 / 分隔，用 path 包而非 filepath（与本地文件系统无关）。
func isTopLevel(cloudRoot, cloudPath string) bool {
	return path.Dir(path.Clean(cloudPath)) == path.Clean(cloudRoot)
}
