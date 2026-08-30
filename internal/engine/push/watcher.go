package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// Watcher 本地实时监听任务（常驻协程）。事件按选项分流：
// 视频/strm 立即同步（对应开关开启时）→ 直传；其余 → 收集父目录进防抖合集后批量扫描。
type Watcher struct {
	paths   *shared.TaskPaths
	rules   shared.Rules
	sc      *Scanner
	co      *CloudOps
	dirPool *DirPool
	running func() bool // 本任务 pull 是否运行中（互斥判定）
	opts    Opts
	arm     func(string)                  // 登记父目录进防抖合集（Pump 构造 batcher 后注入）
	direct  func(context.Context, string) // 监听直传执行器（任务单元注入：开一条执行历史承载直传日志）
}

// NewWatcher 构造监听模块。
func NewWatcher(deps *shared.Deps, sc *Scanner, co *CloudOps, dirPool *DirPool, running func() bool, opts Opts) *Watcher {
	return &Watcher{paths: deps.Paths, rules: deps.Rules, sc: sc, co: co, dirPool: dirPool, running: running, opts: opts}
}

// armParent 登记某路径的父目录进防抖合集。
func (w *Watcher) armParent(p string) {
	if w.arm != nil {
		w.arm(filepath.Dir(p))
	}
}

// OnDirect 注入监听直传执行器：由任务单元提供，负责为单次直传开一条执行历史。
func (w *Watcher) OnDirect(fn func(context.Context, string)) { w.direct = fn }

// cacheExcludeFilter 实现 fswatcher.PathFilter：按路径前缀忽略本地缓存目录子树。
type cacheExcludeFilter struct {
	dir string
}

func (f *cacheExcludeFilter) ShouldInclude(path string) bool {
	return path != f.dir && !strings.HasPrefix(path, f.dir+string(os.PathSeparator))
}

// Pump 文件监听主循环（常驻协程，ctx 取消退出）。
func (w *Watcher) Pump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(w.paths.LocalDir,
			fswatcher.WithPathFilter(&cacheExcludeFilter{dir: w.paths.CacheDir})),
		fswatcher.WithSeverity(fswatcher.SeverityNone),
	)
	if err != nil {
		journal.Error(ctx, "监听器启动失败", "错误", err)
		return
	}
	go func() {
		if werr := watcher.Watch(ctx); werr != nil {
			journal.Error(ctx, "监听器运行异常退出", "错误", werr)
		}
	}()
	journal.Info(ctx, "文件监听器启动", "路径", w.paths.LocalDir)

	batcher := newDirBatcher(func() time.Duration { return w.paths.Debounce })
	w.arm = batcher.Arm

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-batcher.Kick():
				w.flushDirs(ctx, batcher.Take(), batcher.Arm)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			batcher.Stop()
			journal.Info(ctx, "文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			w.dispatch(ctx, ev.Path)
		}
	}
}

// dispatch 分流单个事件：视频/strm 按立即同步开关直传；其余收集父目录进防抖合集。
func (w *Watcher) dispatch(ctx context.Context, p string) {
	if (w.opts.VideoNow && w.rules.IsVideoExt(p)) || (w.opts.StrmNow && shared.IsStrmPath(p)) {
		go w.uploadNow(ctx, p)
		return
	}
	w.arm(filepath.Dir(p))
}

// uploadNow 触发单文件直传：交给注入的执行器（任务单元为它单开一条执行历史）。
func (w *Watcher) uploadNow(ctx context.Context, fPath string) {
	// 本任务 pull 运行中让路：登记父目录进防抖合集，pull 结束后随目录扫描统一处理。
	if w.running() {
		w.arm(filepath.Dir(fPath))
		return
	}
	if w.direct == nil {
		return
	}
	w.direct(ctx, fPath)
}

// DirectFile 直传单个文件：取本地现状 + 索引记录，判定与动作交给扫描器的统一收敛点
// （与全量扫描同一条路径），再等本次上传完成。
// 由任务单元在一条执行历史的上下文中调用（故日志归入该次执行）；防双传交给 uploader 的 inFlight 去重。
func (w *Watcher) DirectFile(ctx context.Context, batch *UpBatch, fPath string) {
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		// 本地已不存在：登记父目录走防抖扫描，并把「已删」交给统一判定点处理
		w.armParent(fPath)
		dbFid, dbSize := w.sc.idx.Get(ctx, fPath)
		w.sc.HandleEntry(ctx, batch, fPath, dbFid, dbSize, nil)
	} else {
		dbFid, dbSize := w.sc.idx.Get(ctx, fPath)
		w.sc.HandleEntry(ctx, batch, fPath, dbFid, dbSize, fileInfoEntry{name: filepath.Base(fPath), info: fileInfo})
	}
	batch.Wait() // 等本次投递的上传完成，保证收尾时日志已齐全
}

// flushDirs 防抖批量处理一批目录：逐个投进目录池，由任务单元消费循环串行消化。
func (w *Watcher) flushDirs(ctx context.Context, folders []string, rearm func(string)) {
	if w.running() {
		for _, f := range folders {
			rearm(f)
		}
		return
	}
	for _, f := range folders {
		if _, statErr := os.Stat(f); statErr != nil {
			journal.Debug(ctx, "待处理目录本地已不存在", "路径", f, "错误", statErr)
			if f != w.paths.LocalDir {
				if parent := filepath.Dir(f); parent != "." {
					rearm(parent)
				}
			}
			continue
		}
		w.dirPool.Enqueue(f, journal.TriggerWatch)
	}
}
