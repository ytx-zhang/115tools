// strm.go 实现 STRM 生成任务：扫描云端媒体库（StrmPath），为视频生成 .strm 索引文件，
// 成功后把原件移入回收目录（避免 Emby 重复扫到）。另含 RegenerateStrmFiles 用到的
// 本地两棵树重写逻辑（纯本地 IO，ExtractPickcode 反向解析）。
package sync

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/logs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// runStrmGen 执行一轮 STRM 生成任务（在 Task 的协程中运行）。
func runStrmGen(ctx context.Context, env *Env, task *Task) {
	logs.Info(logs.ModuleStrm, "开始生成strm文件...")
	start := time.Now()
	defer func() {
		logs.Info(logs.ModuleStrm, "生成strm任务结束", "总数", task.Total(), "耗时", time.Since(start))
	}()

	task.Reset()
	var (
		moveFidsMu sync.Mutex
		moveFids   []string // 顶层目录下的云端 FID，任务成功后统一移入回收目录
		failed     atomic.Bool
	)

	// 只有直接挂在 StrmPath 下的项才登记——生成索引后要从云端原位挪走；
	// 更深层的内容保持原目录结构不动。
	appendMoveFid := func(path, fid string) {
		if filepath.Dir(path) == env.Paths.StrmPath {
			moveFidsMu.Lock()
			moveFids = append(moveFids, fid)
			moveFidsMu.Unlock()
		}
	}

	// StrmPath 的 FID 在 Init 时已查询并缓存，直接复用避免重复 API 调用
	if env.Paths.StrmFid == "" {
		logs.Error(logs.ModuleStrm, "StrmFid 为空，需重新初始化")
		return
	}

	_ = env.WalkCloud(ctx, env.Paths.StrmPath, env.Paths.StrmFid, Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			appendMoveFid(path, fid)
			if err := os.MkdirAll(path, 0755); err != nil {
				logs.Error(logs.ModuleStrm, "创建目录失败", "文件", path, "错误", err)
				failed.Store(true)
				return false, nil
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e Entry) error {
			appendMoveFid(path, fid)
			savePath, _ := ProcessCloudFile(path, e)
			if _, err := os.Stat(savePath); err == nil {
				return nil
			}
			task.AddTotal(1)
			if err := env.FetchAndSave(ctx, pickCode, fid, savePath, e.IsVideo); err != nil {
				failed.Store(true)
				return nil
			}
			task.AddCompleted(1)
			return nil
		},
	}, nil)

	if err := context.Cause(ctx); err != nil {
		logs.Error(logs.ModuleStrm, "生成strm任务被取消", "取消信息", err)
		return
	}
	// 零失败才把原始文件移入回收目录：任一文件生成失败时保持云端原状。
	if !failed.Load() && len(moveFids) > 0 {
		moveStrmPathFiles(ctx, env, moveFids)
	}
}

// moveStrmPathFiles 把收集到的顶层 FID 批量移入云端回收目录（TempFid）。
func moveStrmPathFiles(ctx context.Context, env *Env, fids []string) {
	t0 := time.Now()
	if err := env.API.MoveFile(ctx, strings.Join(fids, ","), env.Paths.TempFid); err != nil {
		logs.Error(logs.ModuleStrm, "移动文件至 TempPath 失败", "错误", err)
	} else {
		logs.Info(logs.ModuleStrm, "移动文件至 TempPath", "文件数量", len(fids), "耗时", time.Since(t0))
	}
}

// regenerateStrmTree 重写某棵本地同步树下的所有 .strm 索引（StrmUrl 变更后调用，
// 纯本地 IO，ExtractPickcode 反向解析旧的 pickcode/fid）。两棵树（SyncPath+StrmPath）并发。
func regenerateStrmTree(ctx context.Context, env *Env, root string) {
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || ctx.Err() != nil {
			return nil
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".strm") {
			return nil
		}
		pickcode, fid := ExtractPickcode(p)
		if pickcode == "" || fid == "" {
			logs.Warn(logs.ModuleStrm, "strm 文件更新时 pickcode 解析失败，跳过", "文件", p)
			return nil
		}
		if err := env.SaveStrmFile(pickcode, fid, p); err != nil {
			logs.Error(logs.ModuleStrm, "重写 strm 文件失败", "文件", p, "错误", err)
			return nil
		}
		// 立即更新数据库版本号（mtime），避免依赖后续扫描兜底刷新时重复比对。
		env.DB.SaveRecord(p, fid, time.Now().Unix())
		return nil
	})
}
