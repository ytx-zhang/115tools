// Package localsync 内文件监听任务：监听 SyncPath 文件事件并分流处理。
// 职责本就属于本地同步（与 Scanner/Uploader 同包，消除跨包调用）。
package localsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// Watcher 本地实时监听任务（连续性任务，由顶层 Runner.Start 启动常驻协程）。
// 事件分流：视频/.strm→异步直传（秒级生效）；其余→收集父目录进防抖合集（默认 10 分钟）后批量扫描。
type Watcher struct {
	paths   *common.Paths
	sc      *Scanner
	co      *CloudOps
	running func() bool  // 云同步是否运行中（顶层注入，供互斥判定）
	arm     func(string) // 登记父目录进防抖合集（Pump 构造 batcher 后注入）
}

// NewWatcher 构造本地实时监听任务（依赖注入）。
func NewWatcher(paths *common.Paths, sc *Scanner, co *CloudOps, running func() bool) *Watcher {
	return &Watcher{paths: paths, sc: sc, co: co, running: running}
}

// armParent 登记某路径的父目录进防抖合集（兜底删除/跨事件一致性）。
func (w *Watcher) armParent(p string) {
	if w.arm != nil {
		w.arm(filepath.Dir(p))
	}
}

// cacheExcludeFilter 实现 fswatcher.PathFilter：按路径前缀直接忽略本地缓存根目录（<SyncPath>/.cache）子树。
// 不用正则（避免转义与误匹配）：目录本身及其下所有子孙路径返回 false（排除），其余 true（包含）。
type cacheExcludeFilter struct {
	dir string
}

// ShouldInclude 实现 fswatcher.PathFilter 接口。
func (f *cacheExcludeFilter) ShouldInclude(path string) bool {
	return path != f.dir && !strings.HasPrefix(path, f.dir+string(os.PathSeparator))
}

// Pump 文件监听器主循环（常驻协程，ctx 取消退出）。
func (w *Watcher) Pump(ctx context.Context) {
	watcher, err := fswatcher.New(
		// ⚠️ 原生忽略 <SyncPath>/.cache 子树：事件在入队前即被 fswatcher 丢弃（watcher.go:738），
		// 既省事件量，又杜绝缓存里的视频被当成新增视频重新上传。路径过滤而非正则，避免转义与误匹配。
		fswatcher.WithPath(w.paths.SyncPath,
			fswatcher.WithPathFilter(&cacheExcludeFilter{dir: w.paths.CacheDir})),
		fswatcher.WithSeverity(fswatcher.SeverityNone), // 关闭 fswatcher 内部日志
	)
	if err != nil {
		logs.Error(logs.ModuleSync, "监听器启动失败", "错误", err)
		return
	}
	go func() {
		if err := watcher.Watch(ctx); err != nil {
			logs.Error(logs.ModuleSync, "监听器运行异常退出", "错误", err)
		}
	}()
	logs.Info(logs.ModuleSync, "文件监听器启动", "路径", w.paths.SyncPath)

	// batcher：事件登记（Arm）→ 防抖到点（Kick）→ 消费者批量取走（Take）。实现见 dirpool.go。
	batcher := &dirBatcher{
		pending: make(map[string]struct{}),
		kick:    make(chan struct{}, 1),
		window:  func() time.Duration { return w.paths.Debounce },
	}
	w.arm = batcher.Arm // 供 dispatch/uploadVideo 共用

	// 防抖后批量扫描目录合集。
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-batcher.Kick():
				w.flushDirs(batcher.Take(), batcher.Arm)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			batcher.Stop()
			logs.Info(logs.ModuleSync, "文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			w.dispatch(ctx, ev.Path)
		}
	}
}

// dispatch 分流单个事件：视频/.strm→go 异步直传（uploadVideo 内调 HandleFile 判定）；其余→收集父目录进防抖合集。
func (w *Watcher) dispatch(ctx context.Context, p string) {
	if w.sc.rules.IsVideoExt(p) || common.IsStrmPath(p) {
		go w.uploadVideo(ctx, p) // 新增/修改秒级生效；删除时 uploadVideo 内 stat 失败走删除分支
		return
	}
	w.arm(filepath.Dir(p))
}

// uploadVideo 单文件视频直传：上传 + 原地转 .strm，秒级生效。
// 只做「确保父目录 + 投递给上传模块」后立刻返回；防双传交给 uploader 的 inFlight 去重。
// 删除事件到达时文件已不存在 → 登记父目录兜底 + 走删除分支清理云端。
func (w *Watcher) uploadVideo(ctx context.Context, fPath string) {
	// 云同步进行中让路：不能丢弃，登记父目录进防抖合集，云同步结束后随目录扫描统一处理。
	if w.running() {
		w.arm(filepath.Dir(fPath))
		return
	}
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		w.armParent(fPath) // 兜底删除/跨事件一致性
		dbFid, dbSize := w.sc.db.GetInfo(fPath)
		w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, nil) // fileInfo==nil → 删云端
		return
	}
	// 先确保父目录云端已建（HandleFile 需经 db.GetFid(dir) 取父 FID 入队）。
	if _, err := w.co.AddCloudFolder(ctx, filepath.Dir(fPath)); err != nil {
		logs.Warn(logs.ModuleSync, "视频直传跳过：无法获取父目录FID", "路径", fPath, "错误", err)
		return
	}
	dbFid, dbSize := w.sc.db.GetInfo(fPath)
	// batch 传 nil = 不入批、不进本地 task 进度/running（静默增量）。仍走统一上传执行器（inFlight 去重、sem 限并发）。
	w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, fileInfo)
}

// flushDirs 防抖批量处理一批目录：逐个投进目录池（去重），由统一工作循环串行消化。
// 已删除的子目录改重登记其父目录（让循环发现子项缺失并清理 DB 孤儿）。
// ⚠️ 云同步进行中让路：整批 rearm 回防抖合集（已 Take 的是防抖合集，rearm 回填即不丢）。
func (w *Watcher) flushDirs(folders []string, rearm func(string)) {
	if w.running() {
		for _, f := range folders {
			rearm(f)
		}
		return
	}
	if len(folders) == 0 {
		return
	}
	for _, f := range folders {
		if _, statErr := os.Stat(f); statErr != nil {
			logs.Debug(logs.ModuleSync, "待处理目录本地已不存在", "路径", f, "错误", statErr)
			if f != w.paths.SyncPath {
				if parent := filepath.Dir(f); parent != "." {
					rearm(parent) // 父目录重新进防抖合集，下次投池
				}
			}
			continue
		}
		w.sc.EnqueueDir(f, SrcWatch) // 投进目录池，由消费者常驻协程串行消化
	}
}
