// 本文件是同步域调度器组合根（Runner）：看这里知道同步怎么运行。
//
// ╔══════════════════════════════════════════════════════════════════════════╗
// ║                            核心数据流                                    ║
// ║   【本地 → 云端】Watcher 监听 → 视频直传/目录防抖 → scanner → uploader   ║
// ║   【云端 → 本地】cronTask 触发 → cloudsyncTask → walker → strmIO          ║
// ║   【STRM 生成】strmgenTask → walker → strmIO → 移入回收                   ║
// ╚══════════════════════════════════════════════════════════════════════════╝
//
// 依赖注入：Runner 持全部基础依赖（api/db/paths/rules），按最小依赖集构造并注入各子包小模块。
package sync

import (
	"context"
	"fmt"
	"sync"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/status"
	"github.com/ytx-zhang/115tools/internal/store"
	"github.com/ytx-zhang/115tools/internal/sync/cloudsync"
	"github.com/ytx-zhang/115tools/internal/sync/common"
	"github.com/ytx-zhang/115tools/internal/sync/localsync"
	"github.com/ytx-zhang/115tools/internal/sync/strmgen"
)

// Runner 同步调度器（组合根）：各任务的装配/启动/触发/互斥/停止唯一视图。
type Runner struct {
	// 基础依赖
	api   *drive.Client
	db    *store.Store
	paths *common.Paths
	rules common.Rules

	// 功能层小模块（子包构造时注入）
	sc   *localsync.Scanner
	up   *localsync.Uploader
	co   *localsync.CloudOps
	wk   *cloudsync.Walker
	strm *cloudsync.StrmIO

	// 任务层：cloudSyncTask/strmGenTask 是「业务体」（Run 所在子包对象）；
	// cloudTask/strmTask/localTask 是「运行态封装」（common.Task：防重入/取消/进度）。
	watcher       *localsync.Watcher
	cron          *cronTask
	cloudSyncTask *cloudsync.Task
	strmGenTask   *strmgen.Task
	cloudTask     *common.Task
	strmTask      *common.Task
	localTask     *common.Task

	// 本地同步专属可取消 ctx：停止本地任务只取消这一路（中断当前扫描+在传上传），
	// 不影响 watcher 常驻监听。localMu 保护下述字段，避免 cron/HTTP handler 与
	// 常驻消费者（newBatchCtx 读 localCtx）并发访问竞争（B2）。
	localMu      sync.Mutex
	localBaseCtx context.Context // 未取消父 ctx（StopTask 重建 localCtx 用）
	localCtx     context.Context // 当前本地同步 ctx（StopTask 取消）；消费者每批从中派生
	localCancel  context.CancelCauseFunc
}

// NewRunner 构造同步调度器（装配全部子包模块）。不启动，调用方再调 Init+Start。
func NewRunner(api *drive.Client, db *store.Store, cfg *config.Config, onChange func()) *Runner {
	paths := common.NewPaths(cfg)
	rules := common.NewRules(cfg)

	r := &Runner{
		api:   api,
		db:    db,
		paths: paths,
		rules: rules,
	}

	// 任务层（一次性任务挂 onChange 广播 SSE 状态；localTask 需先于 sc 存在）
	r.cloudTask = common.NewTask("云端同步", onChange)
	r.strmTask = common.NewTask("STRM 生成", onChange)
	r.localTask = common.NewTask("本地同步", onChange)

	// 子包功能层模块：按最小依赖集注入。
	deps := &common.SyncDeps{API: api, DB: db, Paths: paths, Rules: rules}
	r.strm = cloudsync.NewStrmIO(api, paths)
	r.up = localsync.NewUploader(deps, r.localTask)
	r.co = localsync.NewCloudOps(deps)
	r.wk = cloudsync.NewWalker(deps)
	r.sc = localsync.NewScanner(deps, r.up, r.co, r.localTask)
	r.cloudSyncTask = cloudsync.NewTask(api, db, paths, r.wk, r.strm)
	r.strmGenTask = strmgen.NewTask(api, paths, r.wk, r.strm)
	r.watcher = localsync.NewWatcher(paths, r.sc, r.co, func() bool {
		// 云同步进行中 → 让路（避免并发改同一目录）
		return r.cloudTask.Status().Running
	})
	r.cron = &cronTask{cfg: cfg, runLocalSync: r.startLocalSync, startCloud: r.startCloudSync}

	return r
}

// Init 完成运行时初始化（见 init.go），返回 walked 指示是否执行了 WalkCloud 全量建索引。
func (r *Runner) Init(ctx context.Context) (walked bool, err error) {
	return r.runInit(ctx)
}

// Start 启动常驻协程（watchPump + cronLoop + 本地同步消费者循环 + 首启全量扫描）。
func (r *Runner) Start(ctx context.Context, wg *sync.WaitGroup) {
	// 本地同步专属 ctx：随 StopTask("local") 取消，仅中断「当前批次」扫描与上传，
	// 消费者常驻循环不受影响（挂在顶层 ctx，整体停 Runner 才退出）。
	r.localMu.Lock()
	r.localBaseCtx = ctx
	r.localCtx, r.localCancel = context.WithCancelCause(ctx)
	r.localMu.Unlock()
	wg.Go(func() { r.watcher.Pump(ctx) })
	wg.Go(func() { r.cron.loop(ctx) })
	// 本地同步消费者常驻协程：目录池取目录→处理→等本批上传完；通道非空=running 亮。
	// 挂在顶层 ctx；每取一个目录从可取消 localCtx 派生 per-batch ctx。StopTask 取消 localCtx
	// 只中止当前批次，消费者继续循环；再次启动重建 localCtx 后正常消费。
	wg.Go(func() {
		r.sc.ConsumeLoop(ctx, r.newBatchCtx,
			func() { r.localTask.SetRunning(true) },
			func() { r.localTask.SetRunning(false) },
		)
	})
	// 首启全量扫描：投主同步目录进目录池。
	r.startLocalSync()
}

// newBatchCtx 为一次目录批次派生 per-batch ctx（从可取消 localCtx 派生，停止时本批早退、
// 消费者循环不受影响）。读 localCtx 在 localMu 保护下，避免与 StopTask/startLocalSync 并发写竞争。
func (r *Runner) newBatchCtx() (context.Context, context.CancelFunc) {
	r.localMu.Lock()
	defer r.localMu.Unlock()
	return context.WithCancel(r.localCtx)
}

// startLocalSync 触发本地全量扫描（首启/cron/手动共用）：投主目录进目录池，由消费者串行消化。
// 若已停止（localCancel==nil）先基于 localBaseCtx 重建 localCtx 再投，否则新批次 ScanDir 一进来就 ctx.Err()。
// 云同步进行中让路；SyncFid 未就绪（登录态异常）跳过。
func (r *Runner) startLocalSync() {
	r.localMu.Lock()
	if r.localCancel == nil {
		r.localCtx, r.localCancel = context.WithCancelCause(r.localBaseCtx)
	}
	r.localMu.Unlock()
	if r.cloudTask.Status().Running {
		logs.Info(logs.ModuleSync, "云端同步正在进行，跳过本地全量扫描")
		return
	}
	if r.paths.SyncFid == "" {
		logs.Error(logs.ModuleSync, "主同步目录云端FID未就绪，跳过本地全量扫描", "路径", r.paths.SyncPath)
		return
	}
	r.sc.EnqueueDir(r.paths.SyncPath, localsync.SrcManual)
}

// startCloudSync 触发云端同步任务（cron/手动共用；Task.Start 防重入）。
// 用未取消的 localBaseCtx 派生子 ctx，避免受本地同步停止（localCancel）影响。
func (r *Runner) startCloudSync() {
	r.localMu.Lock()
	base := r.localBaseCtx
	r.localMu.Unlock()
	r.cloudTask.Start(base, func(c context.Context) { r.cloudSyncTask.Run(c, r.cloudTask) })
}

// ──── web 层调用的方法 ────

// StartTask 启动一个任务（name="sync" 云端全量同步 / "strm" STRM 生成 / "local" 本地全量扫描）。
func (r *Runner) StartTask(ctx context.Context, name string) error {
	switch name {
	case "sync":
		r.startCloudSync()
	case "strm":
		r.strmTask.Start(ctx, func(c context.Context) { r.strmGenTask.Run(c, r.strmTask) })
	case "local":
		r.startLocalSync()
	default:
		return fmt.Errorf("未知任务: %s", name)
	}
	return nil
}

// StopTask 停止一个任务（name 同上）。
// 「停止本地同步」= 停止当前扫描 + 中断在传上传 + 清空待扫描目录：
// localCancel(nil) 取消当前批次 → ClearPending 丢弃未消费目录 → SetRunning(false) 立即灭 running
// → localCancel 置 nil 标记已停（下次 startLocalSync 重建）。
// ⚠️ watcher 的 Pump 用顶层 ctx 常驻不受影响，停止后新事件继续入池，下次启动消化。
func (r *Runner) StopTask(name string) {
	switch name {
	case "sync":
		r.cloudTask.Stop()
	case "strm":
		r.strmTask.Stop()
	case "local":
		r.localMu.Lock()
		if r.localCancel != nil {
			r.localCancel(nil) // 中断在途扫描 + 在传上传（仅取消当前批次，消费者循环继续）
			r.localCancel = nil
		}
		r.localMu.Unlock()
		r.sc.ClearPending()
		r.localTask.SetRunning(false)
	}
}

// Status 返回三个任务的进度快照。
func (r *Runner) Status() (cloud, strm, local *status.TaskStatus) {
	return r.cloudTask.Status(), r.strmTask.Status(), r.localTask.Status()
}
