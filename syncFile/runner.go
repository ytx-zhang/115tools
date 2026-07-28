package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"context"
	"log/slog"
	"sync"
)

// Runner 管理 SyncFile 实例的生命周期，支持配置变更后的「热重载」：
// 取消旧实例的 ctx → 等待其全部协程退出 → 用最新配置重建实例。
//
// 【为什么要它】路径类配置（同步目录/strm 目录等）修改后必须重建 SyncFile
// 才能生效；有了 Runner，用户在面板改配置后无需重启程序。
//
// 【web 层怎么用】
//   - Current() 拿当前实例调用门面方法（热重载进行中/重建失败时为 nil）；
//   - TaskCtx() 拿绑定当前实例生命周期的 ctx 去触发任务（热重载时任务随之停止）；
//   - StatsNotify() 拿状态变更通道做 SSE 事件驱动推送；
//   - Reload() 在配置保存后调用（同步阻塞，调用方自行异步）。
type Runner struct {
	appCtx context.Context // 进程级 ctx（程序退出时取消）
	cfg    *config.Config
	api    *drive.Open115
	db     *db.DB
	appWg  *sync.WaitGroup // 全局等待组：实例协程整体挂接于此，保证优雅退出

	// statsCh 状态变更通知通道，跨热重载实例共享：
	// 新实例的进度统计器仍向同一个通道发信号，web 层 SSE 无需感知实例替换。
	statsCh chan struct{}

	mu       sync.Mutex  // 保护 cur/ctx/cancel/wg 的读访问（Current/TaskCtx/StatusSnapshot）
	reloadMu sync.Mutex  // 序列化热重载本身，避免并发 Reload 重复重建实例
	cur      *SyncFile          // 当前实例（热重载进行中为 nil）
	ctx      context.Context    // 当前实例的生命周期 ctx
	cancel   context.CancelFunc // 取消当前实例
	wg       *sync.WaitGroup    // 当前实例的私有等待组（热重载时等它清空）
}

// NewRunner 创建 Runner（此时尚未启动实例，需再调用 Start）。
// 调用方：main。
func NewRunner(appCtx context.Context, cfg *config.Config, api *drive.Open115, boltDB *db.DB, appWg *sync.WaitGroup) *Runner {
	return &Runner{
		appCtx:  appCtx,
		cfg:     cfg,
		api:     api,
		db:      boltDB,
		appWg:   appWg,
		statsCh: make(chan struct{}, 1),
	}
}

// notifyStats 非阻塞地向 statsCh 发送一次状态变更信号
// （通道满时丢弃，前端收到下一次信号时读取的仍是最新快照，最终一致）。
func (r *Runner) notifyStats() {
	select {
	case r.statsCh <- struct{}{}:
	default:
	}
}

// StatsNotify 返回状态变更通知通道，供 web 层 SSE 阻塞监听。
func (r *Runner) StatsNotify() <-chan struct{} {
	return r.statsCh
}

// Start 首次启动同步器实例。
func (r *Runner) Start() error {
	return r.startLocked()
}

// startLocked 创建新实例。
// ⚠️ 关键：New() 内含「云端全量扫描建库 + 本地首次全量扫描」，对大媒体库可能持续数分钟。
// 期间必须释放 mu，否则所有依赖 Current()/TaskCtx() 的接口（保存配置、状态 SSE、任务启停）
// 都会被同一把锁阻塞、表现为「点保存没反应 / 请求一直待处理」。释放后 Current() 短暂返回
// nil，前端据 config_ready 显示「重载中」即可，不影响其它接口响应。
// 自身管理 mu（调用方不要持锁）。
func (r *Runner) startLocked() error {
	r.mu.Lock()
	ctx, cancel := context.WithCancel(r.appCtx)
	wg := &sync.WaitGroup{}
	r.mu.Unlock() // 见上方说明：New() 很慢，先让出读保护锁

	s, err := New(ctx, r.cfg, r.api, r.db, wg, r.statsCh)
	if err != nil {
		cancel()
		wg.Wait()
		return err
	}

	r.mu.Lock()
	r.cur, r.ctx, r.cancel, r.wg = s, ctx, cancel, wg
	r.mu.Unlock()
	// 将实例协程纳入全局等待，确保进程退出前收尾完成
	r.appWg.Go(wg.Wait)
	return nil
}

// Reload 热重载：停止旧实例并用最新配置重建，使路径类配置实时生效。
// 重载期间 Current() 返回 nil，web 层据此提示「未就绪」。
func (r *Runner) Reload() {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	r.mu.Lock()
	if r.cancel != nil {
		slog.Info("[RELOAD] 停止旧同步器实例...")
		r.cur = nil
		r.notifyStats() // 通知前端进入「未就绪」状态
		r.cancel()
		r.wg.Wait() // 等监听器/上传 worker/定时任务全部退出
	}
	r.mu.Unlock() // 见 startLocked 说明：下方 New() 很慢，先把读保护锁让出

	if err := r.startLocked(); err != nil {
		slog.Error("[RELOAD] 同步器重建失败，请检查新配置", "错误信息", err)
		return
	}
	slog.Info("[RELOAD] 配置热重载完成，同步器已重建")
	r.notifyStats() // 通知前端同步器已就绪
}

// Current 返回当前同步器实例；热重载中或重建失败时为 nil。
func (r *Runner) Current() *SyncFile {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur
}

// TaskCtx 返回绑定当前实例生命周期的 ctx（热重载时被取消，正在跑的任务随之停止）。
func (r *Runner) TaskCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return r.appCtx
	}
	return r.ctx
}
