package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/engine/pull"
	"github.com/ytx-zhang/115tools/internal/engine/push"
	"github.com/ytx-zhang/115tools/internal/engine/shared"
	"github.com/ytx-zhang/115tools/internal/index"
	"github.com/ytx-zhang/115tools/internal/journal"
)

// TaskUnit 单个任务的运行时单元：持有本任务的路径/规则/进度，组装 push 与 pull 双方向。
// 任务内互斥：pull 运行时 push 让路（完成当前目录后等待 pull 结束）；任务间互不阻塞。
type TaskUnit struct {
	task conf.Task
	eng  *Engine

	paths *shared.TaskPaths
	prog  *shared.Progress

	wk      *shared.Walker
	sc      *push.Scanner
	co      *push.CloudOps
	up      *push.Uploader
	watcher *push.Watcher
	pull    *pull.Runner
	dirPool *push.DirPool

	residentCtx context.Context // 任务单元常驻 ctx（随任务移除才取消）

	initializing atomic.Bool // 初始化中（已登记但 init 未跑完，期间不可执行）

	// pull 互斥：pullDone 初始为已关闭（表示无 pull 运行）；pull 运行时替换为 open chan，结束 close。
	pullRunning atomic.Bool
	pullMu      sync.Mutex
	pullDone    chan struct{}
	pullCancel  context.CancelCauseFunc

	// push 专属可取消 ctx（停止 push 只取消当前批次，消费者循环常驻）。
	pushMu     sync.Mutex
	pushCtx    context.Context
	pushCancel context.CancelCauseFunc

	// 常驻协程取消（任务单元移除时级联停止 watcher/pushLoop/cronLoop）。
	residentCancel context.CancelFunc
}

// newUnit 组装单个任务运行时单元（由 Engine 调用）。
func (e *Engine) newUnit(task conf.Task) *TaskUnit {
	paths := shared.NewTaskPaths(e.conf, task)
	deps := &shared.Deps{Pan: e.pan, Index: e.idx, Paths: paths, Rules: e.rules, Cache: e.cache}
	pc := task.PushCfg()
	opts := push.Opts{
		GenStrm:  pc.ToStrm,
		ToCache:  pc.ToCache,
		StrmNow:  pc.Watch.StrmNow,
		VideoNow: pc.Watch.VideoNow,
	}

	u := &TaskUnit{
		task:    task,
		eng:     e,
		paths:   paths,
		prog:    shared.NewProgress(e.onChange),
		dirPool: push.NewDirPool(),
	}
	u.wk = shared.NewWalker(deps)
	u.up = push.NewUploader(deps, u.prog, opts)
	u.co = push.NewCloudOps(deps)
	u.sc = push.NewScanner(deps, u.up, u.co)
	u.watcher = push.NewWatcher(deps, u.sc, u.co, u.dirPool, func() bool { return u.pullRunning.Load() }, opts)
	u.watcher.OnDirect(u.runWatchFile)

	// 云端扫描选项按任务类型显式组装（pull 本体 vs push 的连带扫描）
	var pullOpts pull.Options
	if task.Kind == conf.KindPull {
		// pull 任务：以云端为准拉取，不存在冗余概念（本地无完整索引可判定「云端同名但内容不符」），恒关；
		// 下载云端独有是云端扫描的本职，恒开（不暴露开关）。
		pu := task.PullCfg()
		pullOpts = pull.Options{FetchMissing: true, GenStrm: pu.ToStrm, ArchiveToTemp: pu.ArchiveToTemp, UseIndex: false}
	} else {
		// push 任务的「全量扫描后连带云端扫描」：是否下载云端独有由 FetchMissing 控制（关 = 只做冗余检查）；
		// 冗余删除依赖本任务完整的本地索引，故仅此处可配；
		// 归档到回收目录是 pull 任务的收尾动作，连带扫描不做。
		ap := task.AttachCfg()
		pullOpts = pull.Options{
			FetchMissing:  ap.FetchMissing,
			GenStrm:       ap.ToStrm,
			DropRedundant: ap.DropRedundant,
			UseIndex:      true, // 连带扫描跑在 push 任务上：下载结果必须落库，否则下次本地扫描会重传
		}
	}
	u.pull = pull.NewRunner(deps, u.wk, shared.NewStrmIO(deps), pullOpts)
	u.pullDone = make(chan struct{})
	close(u.pullDone)
	return u
}

// init 完成运行时初始化：建本地目录、确保/解析云端根 FID、首次建索引（仅 push）。
//
// 索引只服务于 push 方向（本地→云端的比对与冗余判定）；pull 本体以云端为准把文件落到本地，不落库。
func (u *TaskUnit) init(ctx context.Context) error {
	u.paths.TempFid = u.eng.tempFid
	isPush := u.task.Kind == conf.KindPush

	if err := os.MkdirAll(u.paths.LocalDir, 0o755); err != nil {
		return fmt.Errorf("创建本地目录失败 %s: %w", u.paths.LocalDir, err)
	}

	info, err := u.eng.pan.GetDirInfo(ctx, u.paths.CloudDir)
	if err != nil {
		// 云端根不存在（data=[]）→ 逐级建（push 走会写根索引的 EnsureRoot）
		fid, ferr := u.ensureCloudRoot(ctx, isPush)
		if ferr != nil {
			return ferr
		}
		u.paths.CloudFid = fid
		return nil
	}
	u.paths.CloudFid = info.Fid
	if !isPush {
		return nil
	}

	// 根 FID 变更 → 清空旧索引
	if dbFid := u.eng.idx.GetFid(ctx, u.paths.LocalDir); dbFid != "" && dbFid != info.Fid {
		journal.Info(ctx, "云端目录 FID 变更，清空索引记录", "路径", u.paths.LocalDir)
		u.eng.idx.ClearPaths(ctx, []string{u.paths.LocalDir})
	}
	u.eng.idx.Put(ctx, u.paths.LocalDir, info.Fid, index.SizeDir)

	// 首次（或索引被清空后）构建云端索引
	if u.eng.idx.CountRecursive(ctx, u.paths.LocalDir) == 0 {
		journal.Info(ctx, "首次构建云端索引", "路径", u.paths.LocalDir)
		if err := u.buildIndex(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ensureCloudRoot 确保云端根存在并返回其 FID：push 走 EnsureRoot（顺带写根索引），pull 走不落库版本。
func (u *TaskUnit) ensureCloudRoot(ctx context.Context, isPush bool) (string, error) {
	if !isPush {
		return shared.EnsureCloudDir(ctx, u.eng.pan, u.paths.CloudDir)
	}
	return u.co.EnsureRoot(ctx)
}

// buildIndexProgressStep 索引构建每隔多少条目输出一条进度日志（初始化可能持续数分钟，需有过程可见）。
const buildIndexProgressStep = 2000

// buildIndex 遍历云端树构建本地路径索引（只记 fid/size，不落地文件）。
func (u *TaskUnit) buildIndex(ctx context.Context) error {
	var indexed atomic.Int64 // 遍历回调并发执行，进度计数需原子
	start := time.Now()
	err := u.wk.Walk(ctx, u.paths.CloudDir, u.paths.CloudFid, shared.Visitor{
		EnterDir: func(_ context.Context, path, fid string) (bool, error) {
			local := shared.MapCloudToLocal(u.paths.LocalDir, u.paths.CloudDir, path)
			u.eng.idx.Put(ctx, local, fid, index.SizeDir)
			return true, nil
		},
		VisitFile: func(_ context.Context, path, fid, _ string, e shared.Entry) error {
			local := shared.MapCloudToLocal(u.paths.LocalDir, u.paths.CloudDir, path)
			savePath := local
			saveSize := e.Size
			if e.IsVideo {
				savePath = shared.VideoToStrmPath(local)
				saveSize = time.Now().Unix()
				// 本地已存在旧 strm → 校验归属并规范化链接
				if info, serr := os.Stat(savePath); serr == nil {
					if matched, _, mt := shared.NormalizeOwnedStrm(u.paths.StrmURL, savePath, fid, info.ModTime().Unix()); matched {
						saveSize = mt
					}
				}
			}
			u.eng.idx.Put(ctx, savePath, fid, saveSize)
			if n := indexed.Add(1); n%buildIndexProgressStep == 0 {
				journal.Info(ctx, "云端索引构建中", "已收录", n, "耗时", time.Since(start))
			}
			return nil
		},
	}, func(err error) {
		journal.Error(ctx, "云端索引构建中止", "错误", err)
	})
	if err == nil {
		journal.Info(ctx, "云端索引构建完成", "已收录", indexed.Load(), "耗时", time.Since(start))
	}
	return err
}

// start 启动常驻协程：push 消费者循环 + 监听（仅 push 任务）+ 定时（按类型走 cronLoop）。
// ⚠️ 必须按 Kind 限定：pull 任务启 watcher + pushLoop 会把本地文件误当 push 上传到云端
// （conf 已按类型清理配置段，pull 任务不会残留监听开关；这里保留 Kind 判定作为运行时兜底）。
func (u *TaskUnit) start(ctx context.Context, wg *sync.WaitGroup) {
	u.residentCtx, u.residentCancel = context.WithCancel(ctx)
	if u.task.Kind == conf.KindPush {
		wg.Go(func() { u.pushLoop(u.residentCtx) })
		if u.task.PushCfg().Watch.Enabled {
			wg.Go(func() { u.watcher.Pump(u.residentCtx) })
		}
	}
	if enabled, _ := u.cronSpec(); enabled {
		wg.Go(func() { u.cronLoop(u.residentCtx) })
	}
}

// stop 停止任务单元：取消常驻协程 + 停 push/pull。
func (u *TaskUnit) stop() {
	u.stopPush()
	u.stopPull()
	if u.residentCancel != nil {
		u.residentCancel()
	}
}

// cronSpec 返回本任务的定时配置（push 看 Rescan，pull 看 PullCron）。
func (u *TaskUnit) cronSpec() (enabled bool, intervalHours int) {
	if u.task.Kind == conf.KindPush {
		c := u.task.PushCfg().Rescan
		return c.Enabled, c.IntervalHours
	}
	c := u.task.PullCfg().Cron
	return c.Enabled, c.IntervalHours
}

// pushLoop 常驻消费者：逐目录串行处理，每个目录批次记一条执行历史。
func (u *TaskUnit) pushLoop(residentCtx context.Context) {
	for {
		select {
		case <-residentCtx.Done():
			return
		case dir := <-u.dirPool.Chan():
			trigger := u.dirPool.Take(dir)
			u.runPushBatch(residentCtx, dir, trigger)
		}
	}
}

// keepRun 判定一次执行是否值得留在执行历史里。
//
// 规则：只有手动/定时触发的全量扫描保证留记录（无变化也要能确认「跑过了」）；
// 其余（监听触发的直传与目录批次）没实际上传就不占位，日志转投系统程序日志，
// 否则文件频繁变动时执行历史会被大量「扫描 N 上传 0」刷屏。
func keepRun(trigger journal.Trigger, c journal.Counters) bool {
	return c.Uploaded > 0 || trigger == journal.TriggerCron || trigger == journal.TriggerManual
}

// abandonRun 撤销一次执行记录（不占执行历史，已收集日志转投系统程序日志）。
func (u *TaskUnit) abandonRun(residentCtx context.Context, seq uint64) {
	if err := u.eng.journal.Abandon(seq); err != nil {
		journal.Error(residentCtx, "撤销执行记录失败", "错误", err)
	}
}

// runPushBatch 处理一个目录批次：等 pull 空闲 → 记 Run → ScanDir → 结 Run。
func (u *TaskUnit) runPushBatch(residentCtx context.Context, dir string, trigger journal.Trigger) {
	if !u.waitPullIdle(residentCtx) {
		return
	}
	seq, err := u.eng.journal.Begin(journal.Run{
		TaskID:    u.task.ID,
		TaskName:  u.task.Name,
		Direction: journal.DirPush,
		Trigger:   trigger,
	})
	if err != nil {
		journal.Error(residentCtx, "写入执行记录失败", "错误", err)
		return
	}

	batchCtx, batchCancel := context.WithCancel(u.pushBaseCtx())
	batchCtx = journal.WithTask(batchCtx, u.task.ID, seq)

	u.prog.Reset()
	u.prog.SetRunning(true)
	batch := push.NewUpBatch(true)
	u.sc.ScanDir(batchCtx, dir, batch)
	// ⚠️ 必须在 batchCancel() 之前判定取消：cancel(nil) 之后 Cause 恒为 context.Canceled，
	// 会把所有正常完成的批次错标为「已取消」。
	canceled := context.Cause(batchCtx) != nil
	batchCancel()

	state := journal.StateSuccess
	if canceled {
		state = journal.StateCanceled
	}
	counters := journal.Counters{
		Scanned:  u.prog.Total(),
		Uploaded: u.prog.Completed(),
	}
	if !keepRun(trigger, counters) {
		u.prog.SetRunning(false)
		u.abandonRun(residentCtx, seq)
		return
	}
	// 每次执行必留一条 Info 摘要日志，保证执行历史点开可见（即使本次无文件变化）。
	journal.Info(batchCtx, "本地同步批次完成", "目录", dir, "扫描", counters.Scanned,
		"上传", counters.Uploaded, "状态", state)
	if err := u.eng.journal.Finish(seq, state, counters, ""); err != nil {
		journal.Error(residentCtx, "写入执行结果失败", "错误", err)
	}
	u.prog.SetRunning(false)

	// 全量扫描（cron/手动）后附带云端扫描
	if u.task.AttachEnabled() && (trigger == journal.TriggerCron || trigger == journal.TriggerManual) {
		u.startPull(residentCtx, trigger)
	}
}

// runWatchFile 处理一次「监听直传」：单文件单开一条执行历史，
// 使直传过程（建父目录/判定/上传）的日志进执行明细而非系统程序日志。
// 与目录批次并发执行，故不计入任务进度（count=false），避免互相污染计数。
//
// 只有真正投递了上传的执行才留下记录：其余（本地已删、内容未变、跳过上传等）在结束时撤销，
// 日志转投系统程序日志，避免执行历史被无动作的事件刷屏。
func (u *TaskUnit) runWatchFile(residentCtx context.Context, fPath string) {
	seq, err := u.eng.journal.Begin(journal.Run{
		TaskID:    u.task.ID,
		TaskName:  u.task.Name,
		Direction: journal.DirPush,
		Trigger:   journal.TriggerWatch,
	})
	if err != nil {
		journal.Error(residentCtx, "写入执行记录失败", "错误", err)
		return
	}

	batchCtx, batchCancel := context.WithCancel(u.pushBaseCtx())
	batchCtx = journal.WithTask(batchCtx, u.task.ID, seq)

	batch := push.NewUpBatch(false)
	u.watcher.DirectFile(batchCtx, batch, fPath)
	// ⚠️ 必须在 batchCancel() 之前判定取消：cancel(nil) 之后 Cause 恒为 context.Canceled。
	canceled := context.Cause(batchCtx) != nil
	batchCancel()

	counters := journal.Counters{Scanned: 1, Uploaded: batch.Submitted()}
	// 直传恒为监听触发：没上传（本地已删、内容未变、跳过上传）就不占执行历史。
	if !keepRun(journal.TriggerWatch, counters) {
		u.abandonRun(residentCtx, seq)
		return
	}

	state := journal.StateSuccess
	if canceled {
		state = journal.StateCanceled
	}
	// 必留一条 Info 摘要，保证执行历史点开可见。
	journal.Info(batchCtx, "监听直传完成", "文件", fPath, "上传", counters.Uploaded, "状态", state)
	if err := u.eng.journal.Finish(seq, state, counters, ""); err != nil {
		journal.Error(residentCtx, "写入执行结果失败", "错误", err)
	}
}

// startPush 投递本地全量扫描（手动/首启/cron 共用）。
func (u *TaskUnit) startPush(trigger journal.Trigger) {
	if u.paths.CloudFid == "" {
		journal.Error(u.residentCtx, "云端根 FID 未就绪，跳过本地扫描", "路径", u.paths.LocalDir)
		return
	}
	u.dirPool.Enqueue(u.paths.LocalDir, trigger)
}

// stopPush 停止本地同步：取消当前批次 + 清空待处理目录。
func (u *TaskUnit) stopPush() {
	u.pushMu.Lock()
	if u.pushCancel != nil {
		u.pushCancel(nil)
		u.pushCancel = nil
	}
	u.pushMu.Unlock()
	u.dirPool.Clear()
	u.prog.SetRunning(false)
}

// startPull 启动云端同步（防重入）。
func (u *TaskUnit) startPull(parentCtx context.Context, trigger journal.Trigger) bool {
	if !u.pullRunning.CompareAndSwap(false, true) {
		return false
	}
	ctx, cancel := context.WithCancelCause(parentCtx)
	u.pullMu.Lock()
	u.pullDone = make(chan struct{})
	u.pullCancel = cancel
	u.pullMu.Unlock()

	go func() {
		defer func() {
			u.pullRunning.Store(false)
			u.pullMu.Lock()
			close(u.pullDone)
			u.pullCancel = nil
			u.pullMu.Unlock()
			cancel(nil)
			u.prog.SetRunning(false)
		}()
		u.runPull(ctx, trigger)
	}()
	return true
}

// runPull 执行一轮云端同步并落执行历史。
func (u *TaskUnit) runPull(ctx context.Context, trigger journal.Trigger) {
	seq, err := u.eng.journal.Begin(journal.Run{
		TaskID:    u.task.ID,
		TaskName:  u.task.Name,
		Direction: journal.DirPull,
		Trigger:   trigger,
	})
	if err != nil {
		journal.Error(ctx, "写入执行记录失败", "错误", err)
		return
	}
	ctx = journal.WithTask(ctx, u.task.ID, seq)
	u.prog.Reset()
	u.prog.SetRunning(true)

	var c journal.Counters
	runErr := u.pull.Run(ctx, &c)

	state := journal.StateSuccess
	errMsg := ""
	switch {
	case runErr != nil:
		state = journal.StateFailed
		errMsg = runErr.Error()
	case context.Cause(ctx) != nil:
		state = journal.StateCanceled
	}
	// 扫描数由 pull.Run 自己累加（progress 只服务于 push 上传计数，此处不可覆盖）
	if err := u.eng.journal.Finish(seq, state, c, errMsg); err != nil {
		journal.Error(ctx, "写入执行结果失败", "错误", err)
	}
}

// stopPull 停止云端同步。
func (u *TaskUnit) stopPull() {
	u.pullMu.Lock()
	cancel := u.pullCancel
	u.pullMu.Unlock()
	if cancel != nil {
		cancel(nil)
	}
}

// waitPullIdle 等待本任务 pull 空闲（push 让路）。ctx 取消返回 false。
func (u *TaskUnit) waitPullIdle(ctx context.Context) bool {
	for u.pullRunning.Load() {
		u.pullMu.Lock()
		done := u.pullDone
		u.pullMu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-done:
		}
	}
	return true
}

// pushBaseCtx 返回 push 批次派生 ctx 的父（已停止则重建）。
func (u *TaskUnit) pushBaseCtx() context.Context {
	u.pushMu.Lock()
	defer u.pushMu.Unlock()
	if u.pushCancel == nil {
		u.pushCtx, u.pushCancel = context.WithCancelCause(u.residentCtx)
	}
	return u.pushCtx
}

// cronLoop 定时触发本任务（push=全量扫描，pull=云端同步）。
func (u *TaskUnit) cronLoop(ctx context.Context) {
	_, hours := u.cronSpec()
	ticker := time.NewTicker(time.Duration(hours) * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.trigger(journal.TriggerCron)
		}
	}
}

// trigger 按任务类型触发执行（push 投扫描 / pull 启动同步）。
func (u *TaskUnit) trigger(trigger journal.Trigger) {
	if u.task.Kind == conf.KindPush {
		u.startPush(trigger)
		return
	}
	u.startPull(u.residentCtx, trigger)
}

// runtime 返回任务的运行时状态快照。
func (u *TaskUnit) runtime() TaskRuntime {
	return TaskRuntime{
		ID:           u.task.ID,
		Name:         u.task.Name,
		Type:         string(u.task.Kind),
		Running:      u.prog.Running(),
		Initializing: u.initializing.Load(),
		Completed:    u.prog.Completed(),
		Total:        u.prog.Total(),
	}
}
