// Package strmgen 实现 STRM 生成任务：扫描云端媒体库为视频生成 .strm 索引。
package strmgen

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// Task STRM 生成任务（连续性任务，由顶层 Runner 的 strmTask 驱动）。
// 职责：扫描云端媒体库（StrmPath），为视频生成 .strm 索引文件，
// 成功后把原件移入回收目录（避免 Emby 重复扫到）。
type Task struct {
	api   *drive.Client
	paths *common.Paths
	wk    *common.Walker
	strm  *common.StrmIO
}

// NewTask 构造 STRM 生成任务（依赖注入）。
func NewTask(api *drive.Client, paths *common.Paths, wk *common.Walker, strm *common.StrmIO) *Task {
	return &Task{api: api, paths: paths, wk: wk, strm: strm}
}

// Run 执行一轮 STRM 生成任务（在 Task.Start 的协程中运行）。
func (t *Task) Run(ctx context.Context, task *common.Task) {
	logs.Info(logs.ModuleStrm, "开始STRM 生成", "路径", t.paths.StrmPath)
	start := time.Now()
	defer func() {
		logs.Info(logs.ModuleStrm, "STRM 生成完成", "总数", task.Total(), "耗时", time.Since(start))
	}()

	task.Reset()
	var (
		moveFidsMu sync.Mutex
		moveFids   []string // 顶层目录下的云端 FID，任务成功后统一移入回收目录
		failed     atomic.Bool
	)

	appendMoveFid := func(path, fid string) {
		if filepath.Dir(path) == t.paths.StrmPath {
			moveFidsMu.Lock()
			moveFids = append(moveFids, fid)
			moveFidsMu.Unlock()
		}
	}

	if t.paths.StrmFid == "" {
		logs.Error(logs.ModuleStrm, "StrmFid 为空，需重新初始化")
		return
	}

	if err := t.wk.Walk(ctx, t.paths.StrmPath, t.paths.StrmFid, common.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			appendMoveFid(path, fid)
			if err := os.MkdirAll(path, 0755); err != nil {
				logs.Error(logs.ModuleStrm, "创建目录失败", "路径", path, "错误", err)
				failed.Store(true)
				return false, nil
			}
			return true, nil
		},
		VisitFile: func(ctx context.Context, path, fid, pickCode string, e common.Entry) error {
			appendMoveFid(path, fid)
			savePath, _ := common.ProcessCloudFile(path, e)
			if _, err := os.Stat(savePath); err == nil {
				return nil
			}
			task.AddTotal(1)
			if _, err := t.strm.FetchAndSave(ctx, logs.ModuleStrm, pickCode, savePath, e.IsVideo); err != nil {
				failed.Store(true)
				return nil
			}
			task.AddCompleted(1)
			return nil
		},
	}, nil); err != nil && !errors.Is(err, context.Canceled) {
		logs.Error(logs.ModuleStrm, "云端遍历失败", "路径", t.paths.StrmPath, "错误", err)
	}

	if err := context.Cause(ctx); err != nil {
		logs.Error(logs.ModuleStrm, "STRM 生成被取消", "取消信息", err)
		return
	}
	if !failed.Load() && len(moveFids) > 0 {
		if err := t.moveStrmPathFiles(ctx, moveFids); err != nil {
			logs.Error(logs.ModuleStrm, "移动文件至 TempPath 失败", "错误", err)
			failed.Store(true)
		}
	}
}

// moveStrmPathFiles 把收集到的顶层 FID 一次性批量移入云端回收目录（TempFid）。
// 本项目不会一次移动大量文件，无需分片；失败返回 error 由调用方记录。
func (t *Task) moveStrmPathFiles(ctx context.Context, fids []string) error {
	t0 := time.Now()
	if err := t.api.MoveFile(ctx, strings.Join(fids, ","), t.paths.TempFid, t.paths.StrmPath); err != nil {
		return fmt.Errorf("移动文件至 TempPath 失败: %w", err)
	}
	logs.Info(logs.ModuleStrm, "移动文件至 TempPath", "文件数量", len(fids), "耗时", time.Since(t0))
	return nil
}
