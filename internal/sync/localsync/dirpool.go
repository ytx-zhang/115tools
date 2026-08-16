package localsync

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
)

// 本地同步目录池（生产者-消费者模型）。
// 生产者（Runner 首启/手动/cron、watcher 事件归并）调 EnqueueDir 往目录通道投递目录；
// 常驻消费者 ConsumeLoop 从通道取目录、构造本批 wg、调 ScanDir(dir, wg)、wg.Wait() 等本批上传完，再取下一个。
// 通道非空=本地同步忙（running 亮），消费完变空=空闲（running 灭）。
// 批次 wg 显式透传（不再用 ctx 携带），消费末尾 Wait 即覆盖「扫描+本批上传」。
//
// 该模型已不属于 Scan 行为：Scan 只负责「比对一个目录并投递上传」，目录池负责「调度哪些目录要扫、串行消化、驱动 running 状态」。
// 因此独立成文件，与 scanner.go（纯比对逻辑）解耦。

// dirPool 目录池：待处理目录通道 + 去重 pending，从 Scanner 上提升为独立类型，
// 让「目录调度」职责边界更清晰（见上方「目录池」说明）。EnqueueDir/ClearPending 挂在
// 本类型上；ConsumeLoop 因需驱动 ScanDir/task/db，仍挂在 Scanner 上并复用 dirPool。

// SyncSource 本地同步触发来源：影响「开始/结束」日志的等级与措辞。
type SyncSource int

const (
	// SrcManual 手动「本地同步」按钮 / 首启全量扫描 / cron 定时全量扫描：开始+结束均显示 INFO。
	SrcManual SyncSource = iota
	// SrcWatch 文件监听触发（防抖归并后投池）：开始改 DEBUG；结束若上传数为 0 也改 DEBUG。
	SrcWatch
)

// dirPool 目录池。pending 的值存触发来源（SyncSource），供 ConsumeLoop 在消费时读取，
// 使「同目录已在 pending 时手动来源可升级为 INFO」生效。
type dirPool struct {
	dirCh   chan string // 待处理目录通道（生产者消费目录池）。缓冲 64 削峰。
	pending sync.Map    // 目录去重：dir → SyncSource（已投未消费则合并，避免重复扫）。
}

// EnqueueDir 投递一个待处理目录（去重：已投未消费则合并）。src 标记触发来源，
// 手动来源优先级高于监听（确保手动全量扫描显示 INFO 开始/结束）。
func (p *dirPool) EnqueueDir(dir string, src SyncSource) {
	prev, loaded := p.pending.LoadOrStore(dir, src)
	if loaded {
		// 已投未消费：手动来源升级（监听来源不动）。
		if src == SrcManual {
			if ex, ok := prev.(SyncSource); ok && ex != SrcManual {
				p.pending.Store(dir, SrcManual)
			}
		}
		return
	}
	p.dirCh <- dir
}

// ConsumeLoop 消费者常驻协程：从目录通道取目录、构造本批 wg、ScanDir（含递归下钻+锁外等本批上传）、
// 再取下一个。通道非空=本地同步忙（running 亮），消费完变空=空闲（running 灭）。
// 经 onStart/onDone 回调驱动 running 标志（进入即 running=true、退出即 running=false）。
// 循环挂在 residentCtx 上：仅 Runner 整体停（residentCtx 取消）时才退出，保证消费者常驻——
// 停止本地同步（取消 localCtx）只会中止「当前批次」的 ScanDir，不会让消费者退出（修复 B1）。
// 每轮取到一个目录时，经 newBatchCtx 从可取消的 localCtx 派生 per-batch ctx 传给 ScanDir：
// StopTask 取消 localCtx → 当前批次 ctx 随之取消 → 在途 ScanDir 早退、在传上传由 drive 层中断；
// 批次结束（或早退）后 batchCancel 释放。dirCh 的阻塞发送（EnqueueDir）因此永远有活着的消费者消化。
func (sc *Scanner) ConsumeLoop(residentCtx context.Context, newBatchCtx func() (context.Context, context.CancelFunc), onStart, onDone func()) {
	for {
		select {
		case <-residentCtx.Done():
			return
		case dir, ok := <-sc.dirPool.dirCh:
			if !ok {
				return
			}
			// 消费时读取触发来源（可能已被手动升级为 SrcManual）。
			srcAny, _ := sc.dirPool.pending.LoadAndDelete(dir)
			src := SrcManual
			if s, ok := srcAny.(SyncSource); ok {
				src = s
			}
			onStart()
			sc.task.Reset()
			start := time.Now()
			// 文件监听触发的批次：开始日志降为 DEBUG（手动/首启/cron 仍 INFO）。
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
			sc.ScanDir(batchCtx, dir, true, &batch) // ScanDir 内部锁外 batch.Wait()，覆盖「扫描+本批上传完」
			batchCancel()
			upCount := sc.task.Total() // 本批实际上传文件数
			doneArgs := []any{"路径", dir, "耗时", time.Since(start).String(), "上传文件", upCount}
			// 文件监听触发且本批无上传：结束日志降为 DEBUG（避免无变化时刷 INFO 干扰）；
			// 手动/首启/cron 全量扫描：始终 INFO（开始+结束需可见）。均附上传文件数量。
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
// 事件到来时把父目录登记进合集并重置防抖定时；窗口内无新事件才到点唤醒消费者，
// 由消费者一次性取走整批投池（视频事件不走这里，由 Watcher 直传，秒级生效）。
//
// 从 watch.go 的 Pump 闭包（pendingDirs + armDir + take + gTimer）原样提取为可测类型，
// 语义逐项等价：同一个去重合集、同一个容量 1 的唤醒通道（非阻塞发送，满则丢弃因为
// 「有一次待唤醒」已足够）、同样以 time.AfterFunc(1 小时) 占位启动而真实计时始于首次 Arm、
// 同样每次 Arm 都重置定时（Go 1.23+ 的 Timer 已停止/已触发也可直接 Reset）。
// window 为函数而非固定值：与提取前一样在每次 Reset 时才读取 Paths.Debounce（共享指针语义）。
type dirBatcher struct {
	mu      sync.Mutex
	pending map[string]struct{}  // 待处理目录合集（自动去重）
	kick    chan struct{}        // 到点唤醒通道（容量 1，非阻塞发送）
	timer   *time.Timer          // 防抖定时：每次 Arm 重置
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
func (b *dirBatcher) Arm(dir string) {
	b.mu.Lock()
	b.pending[dir] = struct{}{}
	b.mu.Unlock()
	b.timer.Reset(b.window()) // Go 1.23+ 已停止/已触发的 Timer 可直接 Reset
}

// Take 取出并清空待处理目录合集（返回顺序不定，调用方按集合语义使用）。
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
func (b *dirBatcher) Stop() { b.timer.Stop() }

// ClearPending 停止本地同步时丢弃未处理目录：清空去重标记 + 非阻塞排空通道（避免 watcher
// 后续再投时被旧目录干扰）。⚠️ 已取到正在处理的目录：因 ConsumeLoop 的 ctx 已被 Runner 取消，
// 在途 ScanDir 会 ctx.Err() 早退、在传上传由 drive 层随 ctx 中断，处理很快结束、running 在 onDone 自然灭。
// 通道不关闭（watcher 仍常驻投目录，下次启动重建 localCtx 消化）。
func (sc *Scanner) ClearPending() {
	sc.dirPool.pending.Clear()
	for {
		select {
		case <-sc.dirPool.dirCh:
			// 丢弃未消费目录
		default:
			return
		}
	}
}
