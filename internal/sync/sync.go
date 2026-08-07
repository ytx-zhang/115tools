// sync.go 是同步系统的生命周期管理入口：Syncer 类型对外暴露 Initialize
// （配置变更重建入口）、StartTask/StopTask（手动任务）与 CurrentStatus（轻量状态）。
// 配置校验与状态聚合由 init.Broker 负责，本包仅保留同步运行逻辑。
package sync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
)

// Syncer 管理同步实例生命周期：Initialize 统一处理首次启动与配置变更。
type Syncer struct {
	appCtx context.Context
	cfg    *config.Config
	api    *drive.Open115
	db     *db.DB
	appWg  *sync.WaitGroup
	hub    *logs.Hub

	mu       sync.Mutex // 保护 cur/ctx/cancel/wg/onChange
	reloadMu sync.Mutex // 序列化 Initialize，避免并发重建
	cur      *instance
	ctx      context.Context
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
	onChange func() // 状态变更回调（Broker 注入 publishStatus）
}

// NewSyncer 构造 Syncer（不立即启动，调用方再调 Initialize）。
func NewSyncer(appCtx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, appWg *sync.WaitGroup, hub *logs.Hub) *Syncer {
	return &Syncer{
		appCtx: appCtx,
		cfg:    cfg,
		api:    api,
		db:     boltDB,
		appWg:  appWg,
		hub:    hub,
	}
}

// Initialize 安全关闭旧实例并用最新配置完整初始化。
func (s *Syncer) Initialize() (walked bool, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.shutdownLocked()

	ctx, cancel := context.WithCancel(s.appCtx)
	wg := &sync.WaitGroup{}

	// 装配新实例
	env := NewEnv(s.api, s.db, s.cfg)
	walked, err = env.Init(ctx)
	if err != nil {
		cancel()
		wg.Wait()
		return false, err
	}

	inst := &instance{
		env:       env,
		uploadSem: make(chan struct{}, uploadWorkerCount),
		cloudTask: NewTask("云端同步", s.onChange),
		strmTask:  NewTask("STRM生成", s.onChange),
	}
	inst.Start(ctx, wg)
	wg.Go(func() { inst.cronSync(ctx) })

	s.mu.Lock()
	s.cur, s.ctx, s.cancel, s.wg = inst, ctx, cancel, wg
	cb := s.onChange
	s.mu.Unlock()
	s.appWg.Go(wg.Wait)

	if cb != nil {
		cb()
	}
	return walked, nil
}

// shutdownLocked 取消旧实例 ctx 并等待所有协程安全退出。
// 上传由 syncDir 直接执行（无独立 worker 队列），ctx 取消后 doUpload 会因 ctx.Err() 快速退出。
func (s *Syncer) shutdownLocked() {
	s.mu.Lock()
	cancel := s.cancel
	oldWg := s.wg
	s.cur = nil
	s.ctx = nil
	s.cancel = nil
	s.wg = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}

	logs.Info(logs.ModuleSync, "停止旧同步器实例...")
	cancel()

	if oldWg != nil {
		done := make(chan struct{})
		go func() { oldWg.Wait(); close(done) }()
		select {
		case <-done:
			logs.Info(logs.ModuleSync, "旧实例已安全退出")
		case <-time.After(30 * time.Second):
			logs.Warn(logs.ModuleSync, "旧实例退出超时")
		}
	}
}

// ──── 状态查询 ────

// IsReady 当前运行实例是否已就绪。
func (s *Syncer) IsReady() bool {
	return s.current() != nil
}

// CurrentStatus 返回当前实例的任务进度（无锁原子读取）。
func (s *Syncer) CurrentStatus() (cloud *TaskProgress, strm *TaskProgress, ok bool) {
	cur := s.current()
	if cur == nil {
		return nil, nil, false
	}
	cloud, strm = cur.Status()
	return cloud, strm, true
}

// SetStatusCallback 注册状态变更回调（Broker 注入 publishStatus）。
func (s *Syncer) SetStatusCallback(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// ──── web 层调用的方法 ────

// StartTask 启动一个任务（name="sync" 云端全量同步 / "strm" STRM 生成）。
// 实例为 nil（Initialize 未完成）时返回错误。
func (s *Syncer) StartTask(name string) error {
	cur := s.current()
	if cur == nil {
		return fmt.Errorf("同步器实例未就绪")
	}
	ctx := s.TaskCtx()
	switch name {
	case "sync":
		cur.cloudTask.Start(ctx, func(c context.Context) {
			runCloudSync(c, cur.env, cur.cloudTask)
		})
	case "strm":
		cur.strmTask.Start(ctx, func(c context.Context) {
			runStrmGen(c, cur.env, cur.strmTask)
		})
	default:
		return fmt.Errorf("未知任务: %s", name)
	}
	return nil
}

// StopTask 停止一个任务（name 同上）。
func (s *Syncer) StopTask(name string) {
	cur := s.current()
	if cur == nil {
		return
	}
	switch name {
	case "sync":
		cur.cloudTask.Stop()
	case "strm":
		cur.strmTask.Stop()
	}
}

// RegenerateStrm 在 StrmUrl 变更后重写本地所有 .strm 内容（纯本地 IO）。
func (s *Syncer) RegenerateStrm(ctx context.Context, strmURL string) {
	if cur := s.current(); cur != nil {
		cur.env.Paths.StrmUrl = strmURL
		cur.RegenerateStrmFiles(ctx)
	}
}

// TaskCtx 返回当前实例 ctx；无实例则返回 appCtx。
func (s *Syncer) TaskCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return s.appCtx
	}
	return s.ctx
}

func (s *Syncer) current() *instance {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

// ──── 运行实例 ────

// instance 是单次运行的同步实例：持有运行环境、上传并发信号量、并发去重与两个一次性任务。
// 上传执行模型：syncDir 扫描后直接并发 doUpload（uploadSem 限并发），无独立 worker 池。
type instance struct {
	env       *Env
	uploadSem chan struct{} // 上传并发信号量：目录内并发上限，目录间串行由 syncDir 的 wg.Wait 保证
	inFlight  sync.Map
	cloudTask *Task
	strmTask  *Task
}

// Start 启动后台协程。⚠️ FullScan 必须异步（否则阻塞 Init，前端卡"重载中"）。
func (l *instance) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Go(func() { l.watchPump(ctx) })
	wg.Go(func() { l.FullScan(ctx) })
}

// Status 返回本实例两个任务的进度快照。
func (l *instance) Status() (cloud, strm *TaskProgress) {
	return l.cloudTask.Status(), l.strmTask.Status()
}

// RegenerateStrmFiles 重写两棵本地同步树的 .strm 索引（纯本地 IO）。
func (l *instance) RegenerateStrmFiles(ctx context.Context) {
	wg := &sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		regenerateStrmTree(ctx, l.env, l.env.Paths.SyncPath)
	}()
	go func() {
		defer wg.Done()
		regenerateStrmTree(ctx, l.env, l.env.Paths.StrmPath)
	}()
	wg.Wait()
}

// cronSync 定时全量同步。cron.enabled=false 时挂起空转。
func (l *instance) cronSync(ctx context.Context) {
	if !l.env.CronEnabled {
		logs.Info(logs.ModuleSync, "定时全量同步已关闭，仅依赖本地文件监听")
		<-ctx.Done()
		return
	}
	interval := l.env.CronInterval
	logs.Info(logs.ModuleSync, "定时全量同步已启用", "间隔", interval.String())
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			logs.Info(logs.ModuleSync, "触发定时全量同步任务")
			l.FullScan(ctx)
			l.cloudTask.Start(ctx, func(c context.Context) {
				runCloudSync(c, l.env, l.cloudTask)
			})
		case <-ctx.Done():
			return
		}
	}
}
