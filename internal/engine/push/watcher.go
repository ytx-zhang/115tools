package push

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/engine/kit"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// Watcher 本地实时监听任务（常驻协程）。事件按选项分流：
// 视频/strm 立即同步（对应开关开启时）→ 直传；其余 → 收集父目录进防抖合集后批量扫描。
type Watcher struct {
	paths   *kit.TaskPaths
	rules   kit.Rules
	sc      *Scanner
	co      *CloudOps
	dirPool *DirPool
	running func() bool // 本任务 pull 是否运行中（互斥判定）
	opts    Opts
	arm     func(string) // 登记父目录进防抖合集（Pump 构造 batcher 后注入）
}

// NewWatcher 构造监听模块。
func NewWatcher(deps *kit.Deps, sc *Scanner, co *CloudOps, dirPool *DirPool, running func() bool, opts Opts) *Watcher {
	return &Watcher{paths: deps.Paths, rules: deps.Rules, sc: sc, co: co, dirPool: dirPool, running: running, opts: opts}
}

// armParent 登记某路径的父目录进防抖合集。
func (w *Watcher) armParent(p string) {
	if w.arm != nil {
		w.arm(filepath.Dir(p))
	}
}

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
				w.flushDirs(batcher.Take(), batcher.Arm)
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
	if w.opts.VideoNow && w.rules.IsVideoExt(p) {
		go w.uploadVideo(ctx, p)
		return
	}
	if w.opts.StrmNow && kit.IsStrmPath(p) {
		go w.uploadVideo(ctx, p)
		return
	}
	w.arm(filepath.Dir(p))
}

// uploadVideo 单文件直传：确保父目录 + 投递给上传模块；防双传交给 uploader 的 inFlight 去重。
func (w *Watcher) uploadVideo(ctx context.Context, fPath string) {
	// 本任务 pull 运行中让路：登记父目录进防抖合集，pull 结束后随目录扫描统一处理。
	if w.running() {
		w.arm(filepath.Dir(fPath))
		return
	}
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		w.armParent(fPath)
		dbFid, dbSize := w.sc.vault.Get(ctx, fPath)
		w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, nil)
		return
	}
	if _, err := w.co.AddCloudFolder(ctx, filepath.Dir(fPath)); err != nil {
		journal.Warn(ctx, "视频直传跳过：无法获取父目录 FID", "路径", fPath, "错误", err)
		return
	}
	dbFid, dbSize := w.sc.vault.Get(ctx, fPath)
	w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, fileInfo)
}

// flushDirs 防抖批量处理一批目录：逐个投进目录池，由任务单元消费循环串行消化。
func (w *Watcher) flushDirs(folders []string, rearm func(string)) {
	if w.running() {
		for _, f := range folders {
			rearm(f)
		}
		return
	}
	for _, f := range folders {
		if _, statErr := os.Stat(f); statErr != nil {
			journal.Debug(context.Background(), "待处理目录本地已不存在", "路径", f, "错误", statErr)
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
