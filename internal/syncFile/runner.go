package syncFile

import (
	"context"
	"github.com/ytx-zhang/115tools/internal/config"
	"github.com/ytx-zhang/115tools/internal/db"
	"github.com/ytx-zhang/115tools/internal/drive"
	"github.com/ytx-zhang/115tools/internal/event"
	"log/slog"
	"sync"
	"time"
)

// SyncEvent 是推送给前端的完整状态事件（自带快照，web 无需回拉）。
type SyncEvent struct {
	View *StatusView
}

// Runner 管理 SyncFile 实例的生命周期，支持配置变更后的热重载：
// 取消旧实例 ctx → 带超时有限等待收尾 → 用最新配置重建。
// 对外输出走事件流 events —— web 经 Events() 订阅、Current()/TaskCtx()/Reload() 控制。
type Runner struct {
	appCtx context.Context
	cfg    *config.Config
	api    *drive.Open115
	db     *db.DB
	appWg  *sync.WaitGroup

	events *event.Stream[SyncEvent] // 跨热重载实例共享的状态事件流

	mu       sync.Mutex // 保护 cur/ctx/cancel/wg 读访问
	reloadMu sync.Mutex // 序列化热重载，避免并发 Reload 重复重建
	cur      *SyncFile
	ctx      context.Context
	cancel   context.CancelFunc
	wg       *sync.WaitGroup
}

func NewRunner(appCtx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, appWg *sync.WaitGroup) *Runner {
	return &Runner{
		appCtx: appCtx,
		cfg:    cfg,
		api:    api,
		db:     boltDB,
		appWg:  appWg,
		events: event.New[SyncEvent](16),
	}
}

// Snapshot 返回当前完整状态快照（含 Ready/ConfigReady/Missing/Sync/Strm）。
func (r *Runner) Snapshot() *StatusView {
	r.mu.Lock()
	cur := r.cur
	r.mu.Unlock()
	view := &StatusView{
		Ready:       cur != nil,
		ConfigReady: r.cfg.IsSyncReady(),
		Missing:     r.cfg.RequiredMissing(),
	}
	if cur != nil {
		prog := cur.StatusSnapshot()
		view.Sync = prog.Sync
		view.Strm = prog.Strm
	}
	return view
}

// publishStatus 组装快照并广播一次状态事件（非阻塞，慢订阅者丢事件）。
// 由 cloud/strm 的 onChange 回调及热重载节点调用。
func (r *Runner) publishStatus() {
	r.events.Publish(SyncEvent{View: r.Snapshot()})
}

// Events 返回状态事件订阅通道，供 web 层 SSE 消费（与 fswatcher.Events() 同款手感）。
func (r *Runner) Events() chan SyncEvent { return r.events.Subscribe(16) }

// Unsubscribe 退订状态事件通道。
func (r *Runner) Unsubscribe(ch chan SyncEvent) { r.events.Unsubscribe(ch) }

// Start 首次启动同步器实例。
func (r *Runner) Start() error { return r.startLocked() }

// startLocked 创建新实例。
// ⚠️ New() 含云端全量扫描可达数分钟，期间必须释放 mu，
// 否则 Current()/TaskCtx() 等接口被锁阻塞（v0.8.4 修复的死锁）。
func (r *Runner) startLocked() error {
	r.mu.Lock()
	ctx, cancel := context.WithCancel(r.appCtx)
	wg := &sync.WaitGroup{}
	r.mu.Unlock()

	s, err := New(ctx, r.cfg, r.api, r.db, wg, r.publishStatus)
	if err != nil {
		cancel()
		wg.Wait()
		return err
	}

	r.mu.Lock()
	r.cur, r.ctx, r.cancel, r.wg = s, ctx, cancel, wg
	r.mu.Unlock()
	r.appWg.Go(wg.Wait)
	r.publishStatus() // 首帧：通知 web 实例已就绪
	return nil
}

// Reload 热重载：停止旧实例并用最新配置重建。
// ⚠️ 不用无限 wg.Wait() 等旧实例收尾，给 3s 超时窗口（v0.8.5 修复的卡死）。
// 残留协程随 ctx 取消自行退出，最终收尾由 appWg.Go(wg.Wait) 兜底。
func (r *Runner) Reload() {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	r.mu.Lock()
	oldWg := r.wg
	if r.cancel != nil {
		slog.Info("[RELOAD] 停止旧同步器实例...")
		r.cur = nil
		r.publishStatus() // 通知 web：实例暂不可用（热重载中）
		r.cancel()
	}
	r.mu.Unlock()

	if oldWg != nil {
		done := make(chan struct{})
		go func() { oldWg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			slog.Warn("[RELOAD] 旧实例未在 3s 内退出，强制重建（残留协程将随 ctx 取消自行退出）")
		}
	}

	if err := r.startLocked(); err != nil {
		slog.Error("[RELOAD] 同步器重建失败", "错误信息", err)
		return
	}
	slog.Info("[RELOAD] 配置热重载完成")
	r.publishStatus()
}

// Current 返回当前实例（热重载中为 nil）。
func (r *Runner) Current() *SyncFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// TaskCtx 返回当前实例的 ctx（热重载时被取消）。
func (r *Runner) TaskCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return r.appCtx
	}
	return r.ctx
}
