// sync.go 是同步系统的生命周期管理与编排入口：Syncer 类型吸收原 Runner 与
// SyncFile 两层门面，对外只暴露一组直接方法（StartTask/StopTask/RescanRoot/
// RegenerateStrm/Snapshot/Reload），web 层不再接触内部实例细节。
package sync

import (
	"context"
	"fmt"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"sync"
	"time"
)

// Syncer 管理同步实例生命周期，支持配置热重载：取消旧实例 ctx → 带超时有限等待
// 收尾 → 用最新配置重建。状态经事件流 events 推送给 web 层 SSE。
type Syncer struct {
	appCtx context.Context
	cfg    *config.Config
	api    *drive.Open115
	db     *db.DB
	appWg  *sync.WaitGroup

	hub *logs.Hub // 状态事件经日志 Hub 推前端

	mu       sync.Mutex // 保护 cur/ctx/cancel/wg
	reloadMu sync.Mutex // 序列化热重载，避免并发 Reload 重复重建
	cur      *instance
	ctx      context.Context
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
}

// NewSyncer 构造 Syncer（不立即启动，调用方再调 Start 或 Reload）。
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

// Start 首次启动同步器实例。
func (s *Syncer) Start() error { return s.startLocked("") }

// startLocked 创建新实例。oldSyncPath 用于切换同步目录后清理旧 DB 索引。
// ⚠️ New 含云端全量扫描可达数分钟，期间必须释放 mu，否则 Current/TaskCtx 等被锁阻塞。
func (s *Syncer) startLocked(oldSyncPath string) error {
	s.mu.Lock()
	ctx, cancel := context.WithCancel(s.appCtx)
	wg := &sync.WaitGroup{}
	s.mu.Unlock()

	inst, err := s.newInstance(ctx, wg, oldSyncPath)
	if err != nil {
		cancel()
		wg.Wait()
		return err
	}

	s.mu.Lock()
	s.cur, s.ctx, s.cancel, s.wg = inst, ctx, cancel, wg
	s.mu.Unlock()
	s.appWg.Go(wg.Wait)
	s.publishStatus() // 首帧：通知 web 实例已就绪
	return nil
}

// newInstance 装配并初始化一个同步实例（云端建库 + 本地目录 + 启动后台协程）。
func (s *Syncer) newInstance(ctx context.Context, wg *sync.WaitGroup, oldSyncPath string) (*instance, error) {
	env := NewEnv(s.cfg, s.api, s.db)
	inst := newInstance(env, s.publishStatus)

	if err := inst.ensureDirs(ctx); err != nil {
		return nil, err
	}
	if err := inst.initRoot(ctx, oldSyncPath); err != nil {
		return nil, err
	}
	if err := inst.initTemp(ctx); err != nil {
		return nil, err
	}

	inst.Start(ctx, wg)
	wg.Go(func() { inst.cronSync(ctx) })
	return inst, nil
}

// Reload 热重载：停止旧实例并用最新配置重建。
// ⚠️ 不用无限 wg.Wait() 等旧实例收尾，给 3s 超时窗口（残留协程随 ctx 取消自行退出）。
func (s *Syncer) Reload(oldSyncPath string) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.mu.Lock()
	oldWg := s.wg
	cancel := s.cancel
	if cancel != nil {
		logs.Info(logs.ModuleSync, "停止旧同步器实例...")
		s.cur = nil
	}
	s.mu.Unlock()

	// ⚠️ 必须释放 r.mu 之后再发状态：publishStatus→Snapshot 内部会对 r.mu 取 RLock，
	// 若仍在写锁持有期间调用会触发 RWMutex 不可重入自死锁，热重载永久卡在"停止旧实例"。
	s.publishStatus() // 通知 web：实例暂不可用（热重载中）
	if cancel != nil {
		cancel()
	}

	if oldWg != nil {
		done := make(chan struct{})
		go func() { oldWg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			logs.Warn(logs.ModuleSync, "旧实例未在 3s 内退出，强制重建（残留协程将随 ctx 取消自行退出）")
		}
	}

	if err := s.startLocked(oldSyncPath); err != nil {
		logs.Error(logs.ModuleSync, "同步器重建失败", "错误信息", err)
		return
	}
	logs.Info(logs.ModuleSync, "配置热重载完成")
	s.publishStatus()
}

// ──── web 层调用的方法 ────

// StartTask 启动一个任务（name="sync" 云端全量同步 / "strm" STRM 生成）。
// 热重载中实例为 nil 时返回错误（web 据此返回 503）。
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

// RescanRoot 异步触发一次非递归本地扫描（仅直属子项），用于上传排除名单变更后联动。
func (s *Syncer) RescanRoot(ctx context.Context) {
	if cur := s.current(); cur != nil {
		cur.RescanRoot(ctx)
	}
}

// RegenerateStrm 在 StrmUrl 变更后重写本地所有 .strm 内容（纯本地 IO）。
// ⚠️ 调用前必须把新 URL 传入：Reload 重建实例不重写既有 .strm（扫描只比 mtime），
// 仅 StrmUrl 变不 Reload 时 Env.Paths.StrmUrl 仍是旧值，不更新会导致重写=空转。
func (s *Syncer) RegenerateStrm(ctx context.Context, strmURL string) {
	if cur := s.current(); cur != nil {
		cur.env.Paths.StrmUrl = strmURL
		cur.RegenerateStrmFiles(ctx)
	}
}

// TaskCtx 返回当前实例 ctx（热重载时为已取消 ctx）；无实例则返回 appCtx。
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

// ──── 状态快照与事件流 ────

// StatusView 推送给前端的完整状态快照。
type StatusView struct {
	Ready       bool          `json:"ready"`
	ConfigReady bool          `json:"config_ready"`
	Missing     []string      `json:"missing"`
	Sync        *TaskProgress `json:"sync"`
	Strm        *TaskProgress `json:"strm"`
}

// Snapshot 返回当前完整状态快照（含 Ready/ConfigReady/Missing/Sync/Strm）。
func (s *Syncer) Snapshot() *StatusView {
	cur := s.current()
	view := &StatusView{
		Ready:       cur != nil,
		ConfigReady: s.cfg.IsSyncReady(),
		Missing:     s.cfg.RequiredMissing(),
	}
	if cur != nil {
		view.Sync, view.Strm = cur.Status()
	}
	return view
}

// publishStatus 组装快照并通过 LogStatus 推送前端（非阻塞）。
// 由任务 onChange 回调及热重载节点调用。
func (s *Syncer) publishStatus() {
	snap := s.Snapshot()
	logs.LogStatus(&logs.StatusData{
		Ready:       snap.Ready,
		ConfigReady: snap.ConfigReady,
		Missing:     snap.Missing,
		Sync:        (*logs.TaskStatus)(snap.Sync),
		Strm:        (*logs.TaskStatus)(snap.Strm),
	})
}

// ──── 实例初始化编排见 instance.go（initRoot/initTemp/ensureDirs/cronSync）────
