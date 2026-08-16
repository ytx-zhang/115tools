// Package localsync 内文件监听任务：监听 SyncPath 文件事件并分流处理。
// 职责本就属于本地同步（与 Scanner/Uploader 同包，消除跨包调用）。
package localsync

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/sgtdi/fswatcher"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/sync/common"
)

// Watcher 本地实时监听任务（连续性任务，由顶层 Runner.Start 启动常驻协程）。
// 职责：监听 SyncPath 下的文件事件 → 分流处理：
//   - 视频文件事件：直接 go 异步单文件直传 DoUpload（上传+原地转 .strm），秒级生效；
//   - .strm 文件事件：同样走直传（新增/修改秒级生效），并额外登记父目录兜底删除/一致性；
//   - 其余事件：收集父目录进防抖合集（默认 10 分钟）后批量扫描。
type Watcher struct {
	paths   *common.Paths
	sc      *Scanner
	co      *CloudOps
	running func() bool  // 云同步任务是否运行中（顶层注入，供互斥判定）
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

// Pump 是文件监听器主循环（常驻协程，ctx 取消退出）。
func (w *Watcher) Pump(ctx context.Context) {
	watcher, err := fswatcher.New(
		fswatcher.WithPath(w.paths.SyncPath),
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

	// batcher 待处理目录合集 + 防抖定时 + 唤醒通道：主循环登记（Arm），executor 防抖后批量消费（Take）。
	// 实现见 dirpool.go 的 dirBatcher（从本函数原样提取，语义不变）。防抖窗口每次登记时才读取
	// w.paths.Debounce（Paths 为共享指针，与提取前一致）。
	batcher := &dirBatcher{
		pending: make(map[string]struct{}),
		kick:    make(chan struct{}, 1),
		window:  func() time.Duration { return w.paths.Debounce },
	}
	batcher.timer = time.AfterFunc(time.Hour, batcher.notify) // 占位极大窗口，真实计时始于首次 Arm
	w.arm = batcher.Arm                                       // 注入防抖登记入口，供 dispatch/uploadVideo 共用

	// executor 防抖后批量扫描目录合集。
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

// dispatch 把单个事件分流：视频/.strm → go 异步直传（uploadVideo 内调 HandleFile 判定后入队，DoUpload 内部限流，安全；
// 本地已不存在时 uploadVideo 内部登记父目录兜底删除）；其余事件 → 仅收集父目录进防抖合集。
func (w *Watcher) dispatch(ctx context.Context, p string) {
	if w.sc.rules.IsVideoExt(p) || common.IsStrmPath(p) {
		// 视频/.strm 直传：新增/修改走 uploadVideo 秒级生效；删除时 uploadVideo 内 os.Stat 失败 →
		// 登记父目录 + 走删除分支回收云端。
		go w.uploadVideo(ctx, p)
		return
	}
	w.arm(filepath.Dir(p))
}

// uploadVideo 单文件视频直传：上传 + 原地转 .strm，秒级生效。
// 只做「确保父目录 + 把文件投递给上传模块」，投递后立刻返回（不持锁、不堵塞）。
// 防双传交给 uploader 的 inFlight 去重（与 ScanDir 共用同一上传队列，同一文件不会重复入队）。
// 不判断事件类型：删除事件会让文件在此处 os.Stat 时已经不存在，自然跳过（一次 stat 开销极小）。
func (w *Watcher) uploadVideo(ctx context.Context, fPath string) {
	// 云同步进行中让路：避免与云同步并发改同一目录的云端状态（Watcher.running 由 Runner 注入）。
	// ⚠️ 不能丢弃：登记父目录进防抖合集，等云同步结束后随父目录扫描统一处理（视频事件收集而非丢）。
	if w.running() {
		w.arm(filepath.Dir(fPath))
		return
	}
	// 执行前确认文件仍在：删除/移动走的事件到达时文件已消失，则本地已不存在 →
	// 登记父目录进防抖合集兜底（让 ScanDir 发现 DB 孤儿并回收云端），并走删除分支清理云端。
	fileInfo, err := os.Stat(fPath)
	if err != nil {
		w.armParent(fPath) // 兜底删除/跨事件一致性
		dbFid, dbSize := w.sc.db.GetInfo(fPath)
		w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, nil) // fileInfo==nil → 删云端
		return
	}
	// 先确保父目录云端已建（DB 命中复用 / 缺失才创建），HandleFile 才能经 db.GetFid(dir) 取到父 FID 入队。
	if _, err := w.co.AddCloudFolder(ctx, filepath.Dir(fPath)); err != nil {
		logs.Warn(logs.ModuleSync, "视频直传跳过：无法获取父目录FID", "路径", fPath, "错误", err)
		return
	}
	dbFid, dbSize := w.sc.db.GetInfo(fPath)
	// 视频直传：调 scanner 同一判定入口，batch 传 nil = 不入批、不进 localTask 进度/running（静默增量）。
	// 仍走 uploader.AddUpFile（统一上传执行器），靠 inFlight 去重防双传、靠 sem 限制并发。
	w.sc.HandleFile(ctx, nil, fPath, dbFid, dbSize, fileInfo)
}

// flushDirs 防抖批量处理一批目录：把每个目录投进本地同步目录池（去重），
// 由统一工作循环串行消化（与首启/手动/cron 投主同步目录共用 running/进度，不并发）。
// 已删除的子目录改为重登记其父目录（让循环发现子项缺失并清理 DB 孤儿记录）。
// ⚠️ 此处不再直接调 ScanDir：所有扫描都走目录池→工作循环，保证统一 running + 串行。
func (w *Watcher) flushDirs(folders []string, rearm func(string)) {
	// 云同步进行中让路：避免与云同步并发改同一目录的云端状态（Watcher.running 由 Runner 注入）。
	// ⚠️ 不能丢弃：把整批重新 Arm 回防抖合集（重置定时器），云同步结束后统一处理——
	// 已 Take 清空的是防抖合集，但这里的 folders 是消费者刚取走的批次，rearm 回填即不丢。
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
		w.sc.EnqueueDir(f, SrcWatch) // 投进本地同步目录池（去重），由消费者常驻协程串行消化
	}
}
