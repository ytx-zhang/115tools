package localsync

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// 本地同步目录池（生产者-消费者模型）：Runner 首启/手动/cron 与 watcher 事件归并投递目录，
// 常驻消费者 ConsumeLoop 串行消化（ScanDir + 等本批上传完）。通道非空=本地同步忙（running 亮）。
// 目录调度独立于 Scan（scanner.go 只做目录比对），故本文件独立承载「调度」职责。

// SyncSource 本地同步触发来源：影响「开始/结束」日志的等级与措辞。
type SyncSource int

const (
	// SrcManual 手动「本地同步」按钮 / 首启全量扫描 / cron 定时全量扫描：开始+结束均显示 INFO。
	SrcManual SyncSource = iota
	// SrcWatch 文件监听触发（防抖归并后投池）：开始改 DEBUG；结束若上传数为 0 也改 DEBUG。
	SrcWatch
)

// dirPool 目录池：chan 排队 + pending 去重（dir→来源，已投未消费则合并，手动可升级为 INFO）。
// 去重用 Mutex+map 而非 sync.Map：本场景写多（EnqueueDir 写、ConsumeLoop 删），sync.Map 优势在读多。
type dirPool struct {
	dirCh   chan string           // 待处理目录通道（缓冲 64 削峰）
	mu      sync.Mutex            // 保护 pending
	pending map[string]SyncSource // 去重：dir→来源（未消费则合并）
}

// EnqueueDir 投递目录：已投未消费则合并，手动来源升级（保证全量扫描显示 INFO）。
func (p *dirPool) EnqueueDir(dir string, src SyncSource) {
	p.mu.Lock()
	prev, loaded := p.pending[dir]
	if loaded {
		// 已投未消费：手动来源升级（监听来源不动）。
		if src == SrcManual && prev != SrcManual {
			p.pending[dir] = SrcManual
		}
		p.mu.Unlock()
		return
	}
	p.pending[dir] = src
	p.mu.Unlock()
	p.dirCh <- dir // 解锁后再阻塞发送，避免持锁等消费者
}

// ConsumeLoop 常驻消费者：逐目录串行 ScanDir（含递归下钻+锁外等本批上传）。
// running 经 onStart/onDone 驱动（通道非空=忙）。挂在 residentCtx 上保证常驻：
// 停止本地同步只取消 localCtx → 经 newBatchCtx 派生的当前批次 ctx 早退，消费者不退出。
func (sc *Scanner) ConsumeLoop(residentCtx context.Context, newBatchCtx func() (context.Context, context.CancelFunc), onStart, onDone func()) {
	for {
		select {
		case <-residentCtx.Done():
			return
		case dir, ok := <-sc.dirPool.dirCh:
			if !ok {
				return
			}
			// 消费时读取触发来源（可能已被手动升级为 SrcManual）。取即删，与写入互斥。
			sc.dirPool.mu.Lock()
			src, ok := sc.dirPool.pending[dir]
			delete(sc.dirPool.pending, dir)
			sc.dirPool.mu.Unlock()
			if !ok {
				src = SrcManual // 防御：正常必有记录
			}
			onStart()
			sc.task.Reset()
			start := time.Now()
			// 监听触发的批次降为 DEBUG；手动/首启/cron 保持 INFO。
			if src == SrcWatch {
				logs.Debug(logs.ModuleSync, "开始本地同步", "路径", dir)
			} else {
				logs.Info(logs.ModuleSync, "开始本地同步", "路径", dir)
			}
			if orphans := sc.db.FindOrphanSubdirs(sc.paths.SyncPath); len(orphans) > 0 {
				logs.Info(logs.ModuleSync, "检测到深层孤儿DB记录", "数量", len(orphans))
				sc.db.BatchClearPaths(orphans)
			}
			batchCtx, batchCancel := newBatchCtx()
			var batch sync.WaitGroup
			sc.ScanDir(batchCtx, dir, true, &batch) // ScanDir 内部锁外等本批上传完
			batchCancel()
			upCount := sc.task.Total()
			doneArgs := []any{"路径", dir, "耗时", time.Since(start).String(), "上传文件", upCount}
			// 监听触发且本批无上传：结束降为 DEBUG；否则始终 INFO。
			if src == SrcWatch && upCount == 0 {
				logs.Debug(logs.ModuleSync, "本地同步完成", doneArgs...)
			} else {
				logs.Info(logs.ModuleSync, "本地同步完成", doneArgs...)
			}
			onDone()
		}
	}
}

// ──── 非视频事件的防抖合批器 ────

// dirBatcher 非视频事件（目录/.strm 增删改）的防抖合批器：
// Arm 登记父目录并重置防抖定时；窗口内无新事件才到点唤醒消费者，一次性 Take 整批投池
// （视频事件不走这里，由 Watcher 直传秒级生效）。kick 容量 1 非阻塞发送（有一次待唤醒即够）；
// timer 首次 Arm 时惰性创建（避免占位初始化 hack）；window 为函数，每次 Reset 才读 Paths.Debounce（共享指针语义）。
type dirBatcher struct {
	mu      sync.Mutex
	pending map[string]struct{}  // 待处理目录合集（自动去重）
	kick    chan struct{}        // 到点唤醒通道（容量 1，非阻塞发送）
	timer   *time.Timer          // 防抖定时：每次 Arm 重置（首次 Arm 时 AfterFunc 惰性创建）
	window  func() time.Duration // 防抖窗口取值函数（每次 Reset 时求值）
}

// notify 非阻塞唤醒消费者（通道已有待处理信号则直接丢弃本次通知）。
func (b *dirBatcher) notify() {
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// Arm 登记一个目录并重置防抖定时（合集自动去重）。
// timer 首次 Arm 时惰性创建（⚠️ 必须用 time.AfterFunc 而非 NewTimer：AfterFunc 到期自动调用
// notify 发送 kick 唤醒消费者，NewTimer 只会往无人读取的 timer.C 发值，防抖永不触发）；
// 后续 Arm 直接 Reset（Go 1.23+ 已停止/已触发的 Timer 可直接 Reset）。
// 初始化与 Reset 都持锁，避免多个 dispatch goroutine 并发创建重复 timer。
func (b *dirBatcher) Arm(dir string) {
	b.mu.Lock()
	b.pending[dir] = struct{}{}
	if b.timer == nil {
		b.timer = time.AfterFunc(b.window(), b.notify)
	} else {
		b.timer.Reset(b.window())
	}
	b.mu.Unlock()
}

// Take 取出并清空合集（顺序不定，调用方按集合语义使用）。
func (b *dirBatcher) Take() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	items := slices.Collect(maps.Keys(b.pending))
	clear(b.pending)
	return items
}

// Kick 返回防抖到点的唤醒通道（只读），供监听器的批量消费协程 select。
func (b *dirBatcher) Kick() <-chan struct{} { return b.kick }

// Stop 停止防抖定时（监听器退出时调用）。
func (b *dirBatcher) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
}

// ClearPending 停止本地同步时丢弃未处理目录：清空 pending + 非阻塞排空 dirCh。
// ⚠️ 已取到正在处理的目录不受影响：其 ctx 已被取消，ScanDir 早退后 running 在 onDone 自然灭。
// 通道不关闭（watcher 常驻投目录，下次启动重建 localCtx 消化）。
func (sc *Scanner) ClearPending() {
	sc.dirPool.mu.Lock()
	clear(sc.dirPool.pending)
	sc.dirPool.mu.Unlock()
	for {
		select {
		case <-sc.dirPool.dirCh:
			// 丢弃未消费目录
		default:
			return
		}
	}
}
