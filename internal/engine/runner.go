package engine

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/mirror"
	"github.com/ytx-zhang/115tools/internal/store"
)

// runner 单个任务的运行时：只负责「触发源 → 投 job」。
//
// 不做任何比对与上传（那是 mirror 包的事），也不持有 push/pull 互斥锁——
// 全局单队列天然保证同一时刻只有一份工作在跑。
type runner struct {
	eng   *Engine
	task  conf.Task
	rules mirror.Rules
	prog  *Progress

	stopOnce sync.Once
	cancel   context.CancelFunc // 取消常驻协程（watcher / cron）

	initializing atomic.Bool // 初始化中：已登记但 init 未跑完，此时不可执行

	lastCron  atomicTime // 上次定时触发时刻（推算下次定时）
	lastRun   atomicTime // 上次实际执行完成的时刻
	cronStart atomicTime // 定时器基准锚点（cronLoop 启动时落定，推算未触发时的下次定时）
}

// atomicTime 并发安全的时间戳（零值表示从未发生过）。
type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) Set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }

func (a *atomicTime) Get() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.t
}

// nextCron 推算下次定时触发时刻；未启用定时返回零值。
// 基准取 cronLoop 的实际 ticker 基准（cronStart），保证显示的下次时间与真实触发一致：
// 从未触发过时按锚点推算下一个整间隔时刻，锚点只在启动时落定，刷新页面不会延后。
func (r *runner) nextCron() time.Time {
	if !r.task.Cron.Enabled {
		return time.Time{}
	}
	interval := time.Duration(r.task.CronInterval()) * time.Hour
	if last := r.lastCron.Get(); !last.IsZero() {
		// 触发过：ticker 严格周期，下次 = 上次触发 + 间隔（恰为下一个真实 tick）
		return last.Add(interval)
	}
	base := r.cronStart.Get()
	if base.IsZero() {
		return time.Time{}
	}
	// 下一个大于 now 的整间隔时刻：base + ceil((now-base)/interval)*interval
	elapsed := time.Since(base)
	return base.Add((elapsed/interval + 1) * interval)
}

// newRunner 构造任务运行时（不启动，由 Engine 调 start）。
func (e *Engine) newRunner(task conf.Task) *runner {
	return &runner{
		eng:   e,
		task:  task,
		rules: mirror.NewRules(e.conf.Settings.VideoExts, e.conf.Settings.UploadExclude),
		prog:  NewProgress(e.onChange),
	}
}

// start 启动常驻触发源：文件监听（可选）+ 定时器（可选）。
func (r *runner) start(ctx context.Context) {
	ctx, r.cancel = context.WithCancel(ctx)
	if r.task.WatchEnabled() {
		go r.watchLoop(ctx)
	}
	if r.task.Cron.Enabled {
		go r.cronLoop(ctx)
	}
}

// stop 停止全部常驻触发源（幂等）。
func (r *runner) stop() {
	r.stopOnce.Do(func() {
		if r.cancel != nil {
			r.cancel()
		}
	})
}

// scopes 按任务开关推导本次要跑的作用域。
func (r *runner) scopes() []store.Scope {
	var out []store.Scope
	if r.task.UploadEnabled() {
		out = append(out, store.ScopeUpload)
	}
	if r.task.DownloadEnabled() {
		out = append(out, store.ScopeDownload)
	}
	return out
}

// trigger 按触发方式投递本任务全部作用域的工作。
func (r *runner) trigger(t store.Trigger) {
	for _, sc := range r.scopes() {
		r.eng.queue.Enqueue(Job{TaskID: r.task.ID, Scope: sc, Trigger: t, Dir: r.task.LocalDir})
	}
}

// ──── 定时 ────

// cronLoop 定时触发。
func (r *runner) cronLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(r.task.CronInterval()) * time.Hour)
	defer ticker.Stop()
	r.cronStart.Set(time.Now()) // 记录定时器基准锚点（与 NewTicker 基准对齐）
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			r.lastCron.Set(now)
			r.trigger(store.TriggerCron)
		}
	}
}

// ──── 文件监听 ────

// watchLoop 文件事件监听主循环：事件按开关分流——
// 立即同步命中的文件 → 单文件 job（绕过静默窗口）；其余 → 父目录防抖合批后投目录 job。
func (r *runner) watchLoop(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(r.task.LocalDir,
			fswatcher.WithPathFilter(&excludeFilter{dir: r.eng.cacheDir})),
		fswatcher.WithSeverity(fswatcher.SeverityNone),
	)
	if err != nil {
		slog.ErrorContext(ctx, "监听器启动失败", "错误", err)
		return
	}
	go func() {
		if werr := watcher.Watch(ctx); werr != nil {
			slog.ErrorContext(ctx, "监听器运行异常退出", "错误", werr)
		}
	}()
	slog.InfoContext(ctx, "文件监听器启动", "路径", r.task.LocalDir)

	batcher := newDebouncer(func() time.Duration {
		return time.Duration(r.task.QuietWindow()) * time.Minute
	})
	defer batcher.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-batcher.Kick():
				for _, dir := range batcher.Take() {
					r.eng.queue.Enqueue(Job{TaskID: r.task.ID, Scope: store.ScopeUpload,
						Trigger: store.TriggerWatch, Dir: dir})
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "文件监听器已退出")
			return
		case ev, ok := <-watcher.Events():
			if !ok {
				return
			}
			r.dispatch(batcher, ev.Path)
		}
	}
}

// dispatch 分流单个监听事件。
func (r *runner) dispatch(b *debouncer, path string) {
	switch {
	case r.task.InstantNow && (r.rules.IsVideoExt(path) || strings.EqualFold(filepath.Ext(path), ".strm")):
		// 视频 / .strm 立即同步：单文件 job，走与全量扫描同一条判定路径
		r.eng.queue.Enqueue(Job{TaskID: r.task.ID, Scope: store.ScopeUpload,
			Trigger: store.TriggerWatch, File: path})
	default:
		b.Arm(filepath.Dir(path)) // 其余文件收进父目录的防抖合集
	}
}

// excludeFilter 实现 fswatcher.PathFilter：忽略透传缓存目录子树（避免把缓存当新增上传）。
type excludeFilter struct{ dir string }

func (f *excludeFilter) ShouldInclude(path string) bool {
	if f.dir == "" {
		return true
	}
	return path != f.dir && !strings.HasPrefix(path, f.dir+string(os.PathSeparator))
}

// ──── 防抖合批 ────

// debouncer 非视频事件的防抖合批器：Arm 登记目录并重置定时，窗口内无新事件才到点唤醒。
type debouncer struct {
	mu      sync.Mutex
	pending map[string]struct{}
	kick    chan struct{}
	timer   *time.Timer
	window  func() time.Duration
}

func newDebouncer(window func() time.Duration) *debouncer {
	return &debouncer{pending: make(map[string]struct{}), kick: make(chan struct{}, 1), window: window}
}

// Arm 登记一个目录并重置防抖定时（首次惰性创建，后续 Reset）。
func (b *debouncer) Arm(dir string) {
	b.mu.Lock()
	b.pending[dir] = struct{}{}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.window(), b.notify)
	} else {
		b.timer.Reset(b.window())
	}
	b.mu.Unlock()
}

// Take 取出并清空合集。
func (b *debouncer) Take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, 0, len(b.pending))
	for k := range b.pending {
		out = append(out, k)
	}
	clear(b.pending)
	return out
}

func (b *debouncer) notify() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Kick 返回防抖到点唤醒通道（只读）。
func (b *debouncer) Kick() <-chan struct{} { return b.kick }

// Stop 停止防抖定时。
func (b *debouncer) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}
