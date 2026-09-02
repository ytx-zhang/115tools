// Package engine 是同步引擎的运行时层：管理任务运行时的生命周期、把触发源投递的
// 工作排进全局单队列并逐个执行。
//
// 职责划分：
//   - Engine：任务运行时集合的编排（Init / Reload / 启停 / 状态汇总）+ 全局单消费者循环；
//   - runner：单个任务的触发源（文件监听 / 定时 / 手动），只投 job，不做业务；
//   - Queue：全局单消费者工作队列（去重合并 + 触发方式升级）；
//   - Progress：进度计数器（驱动 SSE 广播）。
//
// 全部业务判定与副作用都在 mirror 包（plan 纯判定 / apply 唯一副作用出口），本包零业务逻辑。
//
// 依赖方向：engine → sync / pan / conf / store；engine 不 import webui。
package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/conf"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/mirror"
	"github.com/ytx-zhang/115tools/internal/store"
)

// TaskRuntime 单任务运行时状态（供 webui 的 SSE 推送与任务卡片展示）。
type TaskRuntime struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Running      bool   `json:"running"`
	Initializing bool   `json:"initializing"` // 初始化中：已登记但未就绪，此时不可执行
	Queued       bool   `json:"queued"`       // 队列中还有该任务的待执行工作
	Completed    int64  `json:"completed"`
	Total        int64  `json:"total"`
	Current      string `json:"current,omitempty"`   // 正在处理的文件
	LastRun      string `json:"last_run,omitempty"`  // 上次执行完成时间（RFC3339）
	NextCron     string `json:"next_cron,omitempty"` // 下次定时触发时间（RFC3339）
}

// Engine 任务引擎：管理全部任务运行时 + 全局单消费者执行循环。
type Engine struct {
	api      *drive.Client
	conf     *conf.Config
	store    *store.Store
	onChange func()
	appCtx   context.Context
	appWg    *sync.WaitGroup

	cacheDir string            // 透传缓存目录（监听过滤器用）
	cache    mirror.CacheMover // 透传缓存写入接口（nil 表示未启用）

	queue *Queue

	mu           sync.Mutex
	runners      map[string]*runner
	progs        map[string]*Progress
	tempFid      string
	started      bool
	activeTask   string             // 当前正在执行 job 所属任务（worker 写）
	activeCancel context.CancelFunc // 当前执行中 job 的取消函数（worker 写、StopTask 读）

	bootstrapMu sync.Mutex // 序列化 EnsureRunning（首次 Init+Start 只执行一次）
}

// New 构造引擎（不启动，调用方再调 EnsureRunning）。
func New(api *drive.Client, cfg *conf.Config, st *store.Store, cacheDir string, cache mirror.CacheMover, onChange func(), appCtx context.Context, appWg *sync.WaitGroup) *Engine {
	return &Engine{
		api:      api,
		conf:     cfg,
		store:    st,
		cacheDir: cacheDir,
		cache:    cache,
		onChange: onChange,
		appCtx:   appCtx,
		appWg:    appWg,
		queue:    NewQueue(),
		runners:  make(map[string]*runner),
		progs:    make(map[string]*Progress),
	}
}

// paths 装配任务的两条路径与运行时 FID。
func (e *Engine) paths(task conf.Task) mirror.Paths {
	return mirror.Paths{
		LocalDir: task.LocalDir,
		CloudDir: conf.CleanCloudPath(task.CloudDir),
		TempFid:  e.tempF(),
		StrmURL:  e.conf.Settings.StrmURL,
		CacheDir: e.cacheDir,
	}
}

// tempF / setTempFid 回收目录 FID 的加锁读写：
// ReloadAll 在 HTTP 请求 goroutine 里重解析，worker 在执行 job 时读取，必须互斥。
func (e *Engine) tempF() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tempFid
}

func (e *Engine) setTempFid(fid string) {
	e.mu.Lock()
	e.tempFid = fid
	e.mu.Unlock()
}

// setActiveCancel 登记当前执行中 job 的取消函数与所属任务（worker 写入）。
func (e *Engine) setActiveCancel(taskID string, cancel context.CancelFunc) {
	e.mu.Lock()
	e.activeTask = taskID
	e.activeCancel = cancel
	e.mu.Unlock()
}

// cancelActive 取消指定任务正在执行的 job（StopTask 调用；不属于该任务则不操作）。
// 仅匹配时清空登记：单协程 worker 内 setActiveCancel/run/defer 串行，下一个 job 的
// 登记一定在当前 defer 之后写入，不会误取消后续 job；不匹配时保留登记，避免破坏
// 其他任务的取消链路。
func (e *Engine) cancelActive(taskID string) {
	e.mu.Lock()
	c, cur := e.activeCancel, e.activeTask
	if cur == taskID {
		e.activeCancel = nil
		e.activeTask = ""
	}
	e.mu.Unlock()
	if c != nil && cur == taskID {
		c()
	}
}

// localCfg / cloudCfg 装配任务两个方向的执行配置。
// ExcludeDir（透传缓存目录）由调用方在装配后覆写：缓存目录只对本地扫描有意义，放这里会误导。
func localCfg(task conf.Task) mirror.LocalCfg {
	return mirror.LocalCfg{ToStrm: task.ToStrm, ToCache: task.ToCache, ExcludeDir: ""}
}

// cloudCfg 装配下载方向配置：下载落地形态用 to_strm_dl；冗余副本清理无配置项，
// 双向（上传+下载同时开）任务自动开启；归档纯下载专用，开启上传时强制关闭（双保险）。
func cloudCfg(task conf.Task) mirror.CloudCfg {
	return mirror.CloudCfg{ToStrm: task.ToStrmDl, DropStale: task.Upload && task.Download,
		Archive: task.Archive && !task.Upload}
}

// rules 装配文件分类规则。
func rulesOf(cfg *conf.Config) mirror.Rules {
	return mirror.NewRules(cfg.Settings.VideoExts, cfg.Settings.UploadExclude)
}

// Init 完成运行时初始化：解析全局回收目录 FID，并为每个启用任务构建运行时。
//
// 索引只服务于「本地 ↔ 云端」比对；首次（或索引被清空后）会重建云端索引，可能耗时数分钟。
func (e *Engine) Init(ctx context.Context) error {
	if err := e.resolveTempFid(ctx); err != nil {
		return err
	}
	for _, t := range e.conf.ListTasks() {
		if !t.Enabled {
			continue
		}
		if _, err := e.initRunner(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// initRunner 登记并初始化一个任务运行时。失败则摘除并带任务名返回。
func (e *Engine) initRunner(ctx context.Context, task conf.Task) (*runner, error) {
	r := e.newRunner(task)
	e.mu.Lock()
	e.runners[task.ID] = r
	e.progs[task.ID] = r.prog
	e.mu.Unlock()
	e.notify()

	start := time.Now()
	slog.InfoContext(ctx, "任务初始化开始", "任务", task.Name, "本地", task.LocalDir, "云端", task.CloudDir)
	r.initializing.Store(true)
	err := e.initTask(ctx, task)
	r.initializing.Store(false)
	if err != nil {
		e.popRunner(task.ID)
		slog.ErrorContext(ctx, "任务初始化失败", "任务", task.Name, "耗时", time.Since(start), "错误", err)
		e.notify()
		return nil, fmt.Errorf("初始化任务 %s 失败: %w", task.Name, err)
	}
	slog.InfoContext(ctx, "任务初始化完成", "任务", task.Name, "耗时", time.Since(start))
	e.notify()
	return r, nil
}

// initTask 单任务的初始化：建本地目录、确保云端根、必要时重建索引。
func (e *Engine) initTask(ctx context.Context, task conf.Task) error {
	paths := e.paths(task)
	if err := os.MkdirAll(paths.LocalDir, 0o755); err != nil {
		return fmt.Errorf("创建本地目录失败 %s: %w", paths.LocalDir, err)
	}
	fid, err := mirror.EnsureCloudDir(ctx, e.api, paths.CloudDir)
	if err != nil {
		return err
	}
	paths.CloudFid = fid

	// 根 FID 变更 → 清空旧索引（云端目录被整体移动 / 删除重建过）
	if dbFid := e.store.Fid(ctx, paths.LocalDir); dbFid != "" && dbFid != fid {
		slog.InfoContext(ctx, "云端目录 FID 变更，清空索引记录", "路径", paths.LocalDir)
		e.store.ClearTree(ctx, paths.LocalDir)
	}
	e.store.Put(ctx, paths.LocalDir, store.Record{Fid: fid, Kind: store.KindDir})

	// 索引只服务于「本地 → 云端」的比对：纯下载任务不建库（预填索引会让下载判定
	// 误判「已同步过、本地已删」而跳过，导致 pull 首次空转）
	if task.UploadEnabled() && e.store.CountRecursive(ctx, paths.LocalDir) == 0 {
		slog.InfoContext(ctx, "首次构建云端索引（可能耗时数分钟）", "路径", paths.LocalDir)
		if err := mirror.BuildIndex(ctx, e.api, e.store, paths, rulesOf(e.conf), task.ToStrm); err != nil {
			return fmt.Errorf("构建云端索引失败: %w", err)
		}
	}
	return nil
}

// resolveTempFid 解析全局回收目录 FID：已存在直接取，否则逐级创建。
func (e *Engine) resolveTempFid(ctx context.Context) error {
	temp := e.conf.Settings.TempDir
	info, err := e.api.GetDirInfo(ctx, temp)
	if err != nil {
		fid, ferr := mirror.EnsureCloudDir(ctx, e.api, temp)
		if ferr != nil {
			return ferr
		}
		e.setTempFid(fid)
		return nil
	}
	e.setTempFid(info.Fid)
	return nil
}

// notify 广播状态变更（SSE 推给前端）。
func (e *Engine) notify() {
	if e.onChange != nil {
		e.onChange()
	}
}

// EnsureRunning 幂等启动引擎：首次调用时 Init + Start，后续调用直接返回。
func (e *Engine) EnsureRunning() error {
	e.bootstrapMu.Lock()
	defer e.bootstrapMu.Unlock()

	e.mu.Lock()
	started := e.started
	e.mu.Unlock()
	if started {
		return nil
	}
	if err := e.Init(e.appCtx); err != nil {
		return err
	}
	var wg sync.WaitGroup
	e.Start(e.appCtx, &wg)
	e.appWg.Go(wg.Wait)
	return nil
}

// ReloadAll 全局设置变更后重建全部任务运行时。
func (e *Engine) ReloadAll() error {
	if err := e.resolveTempFid(e.appCtx); err != nil {
		return err
	}
	for _, t := range e.conf.ListTasks() {
		if err := e.ReloadTask(t); err != nil {
			return err
		}
	}
	return nil
}

// Start 启动所有任务运行时的常驻协程 + 全局单消费者循环。
func (e *Engine) Start(ctx context.Context, wg *sync.WaitGroup) {
	e.mu.Lock()
	e.started = true
	e.mu.Unlock()

	wg.Go(func() { e.worker(ctx) })
	for _, r := range e.snapshotRunners() {
		r.start(ctx)
	}
}

// worker 全局单消费者循环：一次只跑一份工作。
func (e *Engine) worker(ctx context.Context) {
	for {
		job, ok := e.queue.Take(ctx)
		if !ok {
			return
		}
		e.run(ctx, job)
	}
}

// run 执行一份工作，并把结果落成一条活动事件。
func (e *Engine) run(ctx context.Context, job Job) {
	task, ok := e.conf.GetTask(job.TaskID)
	if !ok || !task.Enabled {
		return
	}
	prog := e.prog(job.TaskID)
	prog.SetRunning(true)
	// job 级可取消 context：StopTask 通过取消它中止当前执行；执行结束（成功/失败/取消）
	// 统一清空运行状态，空闲任务 overview 干净（0/0、无 current）。
	jobCtx, cancel := context.WithCancel(ctx)
	e.setActiveCancel(task.ID, cancel)
	defer func() {
		cancel()
		e.cancelActive(task.ID)
		prog.SetRunning(false)
		prog.SetCurrent("")
		prog.Reset(0)
	}()

	start := time.Now()
	stats, err := e.execute(jobCtx, job, task, prog)
	dur := time.Since(start)
	if r := e.runner(job.TaskID); r != nil {
		r.lastRun.Set(time.Now())
	}

	// 「值得看的事件」：没实际动作且不是手动/定时触发，就不占记录（监听事件高频，
	// 记了会把执行记录刷屏——这也是旧版不得不引入 Abandon「先写再删」的根因，现在根本不写）。
	if stats.Empty() && job.Trigger != store.TriggerManual && job.Trigger != store.TriggerCron {
		return
	}

	ev := store.Event{
		TaskID:     task.ID,
		TaskName:   task.Name,
		Scope:      job.Scope,
		Trigger:    job.Trigger,
		State:      store.StateSuccess,
		Stats:      stats,
		DurationMs: dur.Milliseconds(),
	}
	if err != nil {
		if jobCtx.Err() != nil {
			ev.State = store.StateCanceled
		} else {
			ev.State = store.StateFailed
		}
		ev.Error = err.Error()
	}
	if _, aerr := e.store.Append(ctx, ev); aerr != nil {
		slog.ErrorContext(ctx, "写入活动事件失败", "错误", aerr)
	}
	e.notify()
}

// execute 执行一份工作的业务部分：按方向拆到独立方法（plan → apply）。
func (e *Engine) execute(ctx context.Context, job Job, task conf.Task, prog *Progress) (store.Stats, error) {
	switch job.Scope {
	case store.ScopeUpload:
		return e.runUpload(ctx, job, task, prog)
	case store.ScopeDownload:
		return e.runDownload(ctx, task, prog)
	default:
		return store.Stats{}, fmt.Errorf("未知作用域: %v", job.Scope)
	}
}

// runUpload 本地 → 云端：PlanLocal（或监听直传的 PlanFile）→ Apply。
func (e *Engine) runUpload(ctx context.Context, job Job, task conf.Task, prog *Progress) (store.Stats, error) {
	var stats store.Stats
	rules := rulesOf(e.conf)
	paths := e.paths(task)
	paths.CloudFid = e.store.Fid(ctx, paths.LocalDir)
	if paths.CloudFid == "" {
		return stats, fmt.Errorf("云端根 FID 未就绪，跳过本地扫描")
	}
	cfg := localCfg(task)
	cfg.ExcludeDir = e.cacheDir

	var ops []mirror.Op
	var err error
	if job.File != "" {
		ops, err = mirror.PlanFile(ctx, paths, job.File, e.store, rules, cfg)
	} else {
		root := job.Dir
		if root == "" {
			root = paths.LocalDir
		}
		// paths.LocalDir 保持任务根不变：子目录 job 的云端路径映射依赖它
		ops, err = mirror.PlanLocal(ctx, root, paths, e.store, rules, cfg)
	}
	if err != nil {
		return stats, err
	}
	// 处理目标（动态展示用，最多 1 条）：单文件=具体文件、目录事件=该目录、全量扫描=任务根
	target := paths.LocalDir
	if job.File != "" {
		target = job.File
	} else if job.Dir != "" && job.Dir != paths.LocalDir {
		target = job.Dir
	}
	if aerr := mirror.NewApplier(e.api, e.store, paths, rules, e.cache, prog).Apply(ctx, ops, cfg, &stats); aerr != nil {
		return stats, aerr
	}
	if !stats.Empty() {
		stats.Dirs = []string{target}
	}
	return stats, nil
}

// runDownload 云端 → 本地：扫本地目录数 → ScanCloud（数量一致跳过）→ PlanCloud → Apply。
//
// 不需要 job：下载方向始终以任务配置的云端根（paths.CloudDir）为起点全量比对，
// 不存在「按监听目录/单文件缩小范围」的语义（watch 事件只投上传作用域，见 runner.dispatch）。
func (e *Engine) runDownload(ctx context.Context, task conf.Task, prog *Progress) (store.Stats, error) {
	var stats store.Stats
	rules := rulesOf(e.conf)
	paths := e.paths(task)
	fid, err := mirror.EnsureCloudDir(ctx, e.api, paths.CloudDir)
	if err != nil {
		return stats, err
	}
	paths.CloudFid = fid

	localCount, err := mirror.LocalTreeCount(ctx, paths.LocalDir, rules, e.cacheDir)
	if err != nil {
		return stats, err
	}
	tree, err := mirror.ScanCloud(ctx, e.api, paths, localCount)
	if err != nil {
		return stats, err
	}
	ops, err := mirror.PlanCloud(ctx, tree, e.store, rules, paths, cloudCfg(task))
	if err != nil {
		return stats, err
	}
	// 处理目标：云端同步记录配置的云端最父目录
	if aerr := mirror.NewApplier(e.api, e.store, paths, rules, e.cache, prog).Apply(ctx, ops, localCfg(task), &stats); aerr != nil {
		return stats, aerr
	}
	if !stats.Empty() {
		stats.Dirs = []string{paths.CloudDir}
	}
	return stats, nil
}

// DryRun 只算计划不执行，返回将要发生的动作清单（预演）。
//
// 预演路径**必须零副作用**：云端目录用 GetDirInfo 只读解析（不存在就报错），
// 绝不能调 EnsureCloudDir 去建目录——那是执行阶段才有资格做的事。
func (e *Engine) DryRun(id string, scope store.Scope) ([]mirror.Op, error) {
	task, ok := e.conf.GetTask(id)
	if !ok {
		return nil, fmt.Errorf("任务不存在: %s", id)
	}
	if e.Initializing(id) {
		return nil, fmt.Errorf("任务初始化中，索引未就绪，暂不可预演")
	}
	paths := e.paths(task)
	if scope == store.ScopeUpload {
		paths.CloudFid = e.store.Fid(e.appCtx, paths.LocalDir)
		// 与真实执行（execute）同一口径：排除透传缓存目录，否则缓存视频会被当成本地新增误报上传
		cfg := localCfg(task)
		cfg.ExcludeDir = e.cacheDir
		return mirror.PlanLocal(e.appCtx, paths.LocalDir, paths, e.store, rulesOf(e.conf), cfg)
	}
	info, err := e.api.GetDirInfo(e.appCtx, paths.CloudDir)
	if err != nil {
		return nil, fmt.Errorf("云端目录不存在，无法预演: %w", err)
	}
	paths.CloudFid = info.Fid
	// 与真实执行（runDownload）同一口径：本地云端数量一致则跳过遍历，预演展示与执行结果对齐
	localCount, err := mirror.LocalTreeCount(e.appCtx, paths.LocalDir, rulesOf(e.conf), e.cacheDir)
	if err != nil {
		return nil, err
	}
	tree, err := mirror.ScanCloud(e.appCtx, e.api, paths, localCount)
	if err != nil {
		return nil, err
	}
	return mirror.PlanCloud(e.appCtx, tree, e.store, rulesOf(e.conf), paths, cloudCfg(task))
}

// ──── 生命周期管理 ────

// ReloadTask 热重建单个任务：停旧运行时，按新配置重建并启动（不影响其他任务）。
func (e *Engine) ReloadTask(task conf.Task) error {
	e.mu.Lock()
	started := e.started
	e.mu.Unlock()

	e.queue.DropTask(task.ID)
	if old := e.popRunner(task.ID); old != nil {
		old.stop()
	}
	if !task.Enabled {
		e.notify()
		return nil
	}

	r, err := e.initRunner(e.appCtx, task)
	if err != nil {
		return err
	}
	if started {
		r.start(e.appCtx)
	}
	e.notify()
	return nil
}

// RemoveTask 停止并移除单个任务运行时（配合 conf.RemoveTask 使用）。
func (e *Engine) RemoveTask(id string) {
	e.queue.DropTask(id)
	if old := e.popRunner(id); old != nil {
		old.stop()
	}
	e.mu.Lock()
	delete(e.progs, id)
	e.mu.Unlock()
	e.notify()
}

// StartTask 手动执行任务。
func (e *Engine) StartTask(id string) error {
	e.mu.Lock()
	r := e.runners[id]
	e.mu.Unlock()
	if r == nil {
		return fmt.Errorf("任务未就绪: %s", id)
	}
	if r.initializing.Load() {
		return fmt.Errorf("任务初始化中，索引未就绪，暂不可执行")
	}
	r.trigger(store.TriggerManual)
	return nil
}

// StopTask 停止任务：丢弃排队工作并停掉常驻触发源（监听 / 定时）。
// 与旧版的 stopPush/stopPull 双方向方法合并成一个。
func (e *Engine) StopTask(id string) {
	e.mu.Lock()
	r := e.runners[id]
	e.mu.Unlock()
	if r == nil {
		return
	}
	e.queue.DropTask(id)
	r.stop()
	e.cancelActive(id) // 取消该任务正在执行的 job（若正在跑）
	slog.InfoContext(e.appCtx, "任务已停止", "任务", r.task.Name)
}

// Status 返回所有任务运行时的状态快照。
func (e *Engine) Status() []TaskRuntime {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TaskRuntime, 0, len(e.runners))
	for id, r := range e.runners {
		running, completed, total, current := r.prog.Snapshot()
		rt := TaskRuntime{
			ID:           id,
			Name:         r.task.Name,
			Running:      running,
			Initializing: r.initializing.Load(),
			Queued:       e.queue.HasPending(id),
			Completed:    completed,
			Total:        total,
			Current:      current,
		}
		if t := r.lastRun.Get(); !t.IsZero() {
			rt.LastRun = t.Format(time.RFC3339)
		}
		if t := r.nextCron(); !t.IsZero() {
			rt.NextCron = t.Format(time.RFC3339)
		}
		out = append(out, rt)
	}
	return out
}

// Shutdown 停止所有任务运行时并关闭队列。
func (e *Engine) Shutdown() {
	e.queue.Close()
	for _, r := range e.snapshotRunners() {
		r.stop()
	}
}

// runner 返回某任务的运行时（不存在返回 nil）。
func (e *Engine) runner(id string) *runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runners[id]
}

// snapshotRunners 返回任务运行时快照（加锁拷贝，遍历时不持锁）。
func (e *Engine) snapshotRunners() []*runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*runner, 0, len(e.runners))
	for _, r := range e.runners {
		out = append(out, r)
	}
	return out
}

// popRunner 摘除并返回任务运行时（不存在返回 nil）。
func (e *Engine) popRunner(id string) *runner {
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.runners[id]
	delete(e.runners, id)
	delete(e.progs, id)
	return old
}

// prog 返回任务的进度计数器（不存在则建一个空的，避免空指针）。
func (e *Engine) prog(id string) *Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.progs[id]; ok {
		return p
	}
	p := NewProgress(e.onChange)
	e.progs[id] = p
	return p
}

// HasRunner 任务是否已就绪（前端据此判断可否执行）。
func (e *Engine) HasRunner(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.runners[id]
	return ok
}

// Initializing 任务是否处于初始化中（索引未就绪，预演与执行必须拒绝）。
func (e *Engine) Initializing(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.runners[id]
	return ok && r.initializing.Load()
}
