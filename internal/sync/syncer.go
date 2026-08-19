// Package sync 实现本地文件系统 ↔ 115 网盘的双向同步。
//
// 目录结构（组合根 + 任务子包）：
//   - 顶层（本包）：syncer.go（门面 Syncer）+ runner.go（组合根 Runner）+ cron.go + init.go
//   - common/：零依赖公共值对象与纯函数（Rules/Paths/Entry/Visitor/Task）
//   - localsync/：本地→云端（Scanner/Uploader/CloudOps/Watcher 实时监听）
//   - cloudsync/：云端→本地（Walker/CloudsyncTask；落地编排用 common.StrmIO）
//   - strmgen/：STRM 生成（StrmgenTask）
//
// 数据流图见 runner.go 顶部。
package sync

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/logs"
	"github.com/ytx-zhang/115tools/internal/status"
	"github.com/ytx-zhang/115tools/internal/store"
)

// Syncer 管理同步实例生命周期：Initialize 统一处理首次启动与配置变更。
type Syncer struct {
	appCtx context.Context
	cfg    *config.Config
	api    *drive.Client
	db     *store.Store
	appWg  *sync.WaitGroup

	mu       sync.Mutex // 保护 cur/ctx/cancel/wg/onChange
	reloadMu sync.Mutex // 序列化 Initialize，避免并发重建
	cur      *Runner
	ctx      context.Context
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
	onChange func() // 状态变更回调（app 层注入 publishStatus）
}

// NewSyncer 构造 Syncer（不立即启动，调用方再调 Initialize）。
func NewSyncer(appCtx context.Context, cfg *config.Config, api *drive.Client, boltDB *store.Store, appWg *sync.WaitGroup) *Syncer {
	return &Syncer{
		appCtx: appCtx,
		cfg:    cfg,
		api:    api,
		db:     boltDB,
		appWg:  appWg,
	}
}

// Initialize 安全关闭旧实例并用最新配置完整初始化（Runner 构建 → Init → Start）。
func (s *Syncer) Initialize() (walked bool, err error) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()

	s.shutdownLocked()

	ctx, cancel := context.WithCancel(s.appCtx)
	wg := &sync.WaitGroup{}

	runner := NewRunner(s.api, s.db, s.cfg, s.onChange)
	walked, err = runner.Init(ctx)
	if err != nil {
		cancel()
		wg.Wait()
		return false, err
	}
	runner.Start(ctx, wg)

	s.mu.Lock()
	s.cur, s.ctx, s.cancel, s.wg = runner, ctx, cancel, wg
	cb := s.onChange
	s.mu.Unlock()
	s.appWg.Go(wg.Wait)

	if cb != nil {
		cb()
	}
	return walked, nil
}

// shutdownLocked 取消旧实例 ctx 并等待所有协程安全退出。
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

	logs.Info(logs.ModuleSystem, "停止旧同步器实例...")
	cancel()

	if oldWg != nil {
		done := make(chan struct{})
		go func() { oldWg.Wait(); close(done) }()
		select {
		case <-done:
			logs.Info(logs.ModuleSystem, "旧实例已安全退出")
		case <-time.After(30 * time.Second):
			logs.Warn(logs.ModuleSystem, "旧实例退出超时")
		}
	}
}

// ──── 状态查询 ────

// CurrentStatus 返回当前实例的任务进度。
func (s *Syncer) CurrentStatus() (cloud, strm, local *status.TaskStatus, ok bool) {
	cur := s.current()
	if cur == nil {
		return nil, nil, nil, false
	}
	cloud, strm, local = cur.Status()
	return cloud, strm, local, true
}

// SetStatusCallback 注册状态变更回调（app 层注入 publishStatus）。
func (s *Syncer) SetStatusCallback(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// ──── web 层调用的方法 ────

// StartTask 启动一个任务（name="sync" 云端全量同步 / "strm" STRM 生成）。
func (s *Syncer) StartTask(name string) error {
	cur := s.current()
	if cur == nil {
		return fmt.Errorf("同步器实例未就绪")
	}
	return cur.StartTask(s.taskCtx(), name)
}

// StopTask 停止一个任务（name 同上）。
func (s *Syncer) StopTask(name string) {
	if cur := s.current(); cur != nil {
		cur.StopTask(name)
	}
}

// taskCtx 返回当前实例 ctx；无实例则返回 appCtx。
func (s *Syncer) taskCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil {
		return s.appCtx
	}
	return s.ctx
}

func (s *Syncer) current() *Runner {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}
