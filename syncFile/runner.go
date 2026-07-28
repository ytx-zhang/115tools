package syncFile

import (
	"115tools/config"
	"115tools/db"
	"115tools/drive"
	"context"
	"log/slog"
	"sync"
	"time"
)

// Runner 管理 SyncFile 实例的生命周期，支持配置变更后的「热重载」：
// 取消旧实例的 ctx（监听器/上传 worker/定时任务/云端同步随之退出）→ 带超时
// 有限等待旧实例收尾 → 用最新配置重建实例。旧实例残留协程会随 ctx 取消自行退出，
// 最终收尾由该实例 New() 时登记的 appWg.Go(r.wg.Wait) 兜底（进程退出前等完即可）。
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
// ⚠️ 关键：New() 内含「云端全量扫描建库（initRoot）+ 启动本地同步模块」。其中本地首次
// 全量扫描（FullScan）已改为后台异步（见 local.Start），不阻塞 New() 返回；但 initRoot
// 的云端扫描对大媒体库仍可能持续数分钟，期间必须释放 mu，否则所有依赖 Current()/TaskCtx()
// 的接口（保存配置、状态 SSE、任务启停）都会被同一把锁阻塞、表现为「点保存没反应 / 请求
// 一直待处理」。释放后 Current() 短暂返回 nil，前端据 config_ready 显示「重载中」即可，
// 不影响其它接口响应。自身管理 mu（调用方不要持锁）。
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
	oldWg := r.wg // 旧实例的私有等待组，下方带超时等待其收尾
	if r.cancel != nil {
		slog.Info("[RELOAD] 停止旧同步器实例...")
		r.cur = nil
		r.notifyStats() // 通知前端进入「未就绪」状态
		r.cancel()      // 取消旧实例 ctx：监听器/上传 worker/定时任务/云端同步全部随之退出
	}
	r.mu.Unlock() // 见 startLocked 说明：下方 New() 很慢，先把读保护锁让出

	// 不在此无限阻塞等待旧实例收尾（原 r.wg.Wait()）：旧实例所有协程都随上面的
	// ctx 取消而退出，其最终收尾由该实例 New() 时登记的 appWg.Go(r.wg.Wait) 兜底
	// （进程退出前等完即可）。若改为阻塞等待，一旦旧实例有协程未及时退出
	// （例如 115 接口慢、或未来某路径未尊重 ctx 取消，典型如「还有上传任务在跑」），
	// 热重载就会永久卡在「重建中」——前端一直显示「配置热重载中，同步器正在重建」，
	// 且因其运行在后台 goroutine，连日志都看不出卡在哪。
	// 故改用「带超时的有限等待」：给旧实例 3s 收尾窗口，超时即放手重建；残留协程
	// 随后会在 ctx 取消下自行退出。新建实例与残留旧协程并发无害——二者共用同一
	// boltDB，bbolt 已串行化读写，不存在竞态损坏。
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
