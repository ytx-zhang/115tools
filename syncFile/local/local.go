// Package local 是「本地同步」模块：负责把本地文件的变化同步到 115 云端
// （本地 → 云端 的上传方向）。
//
// 【数据流】
//  1. 文件监听器（watcher.go 的 watchPump）发现本地文件变动，
//     只把事件对应的「父目录」登记进待处理表（内存 map）；
//  2. 事件不是立即处理，而是等待一个 debounce 静默窗口：
//     窗口期内不断有新事件就持续重计时，直到文件真正消停才处理，
//     避免「文件还在拷贝中就开始上传」；
//  3. 静默到期：对每个「云端已存在」的父目录跑一遍 syncDir（scan.go）递归同步；
//     syncDir 是唯一的「处理核心」，与全量扫描/定时扫描共用，保证行为完全一致；
//  4. 需要上传的文件投递到 uploadJobs 队列（upload.go），
//     由常驻上传 worker 并发执行真正的上传；
//  5. 需要在云端建目录/删文件的操作走 cloudops.go。
//
// 【极简事件处理】
// 监听器完全「不懂」业务逻辑：无视事件类型（连 rename/move 也走 syncDir），
// 只记父目录。因为 syncDir 递归，只要某个上层祖先在云端存在就一并补上；
// 云端还不存在的父目录跳过，反正它们自己（或祖先）迟早也会进来被处理。
//
// 【与其他模块的边界】
//   - 本模块只认「本地 → 云端」方向；「云端 → 本地」是 cloud 模块的事；
//   - 共享设施来自 core.Env；与 cloud 模块不加锁互斥，靠幂等避让。
package local

import (
	"115tools/syncFile/core"
	"context"
	"sync"
	"sync/atomic"
)

// Local 是本地同步模块的实例，持有本模块的全部运行时状态。
// 通过 New 构造、Start 启动；后台协程随 ctx 取消而全部退出。
type Local struct {
	env *core.Env // 共享运行环境（API/DB/路径配置，见 core 包）

	// ── 待处理文件夹（watcher.go）──
	// 监听器把事件父目录收进 pending；debounce 静默后由 processPending 逐个 syncDir。
	pendingMu    sync.Mutex          // 保护 pending
	pending      map[string]struct{} // 待处理文件夹（事件父目录）
	pendingDirty atomic.Bool         // 处理期间来了新事件 → 中断当前 syncDir 并把剩余回写

	// ── 上传任务队列（upload.go）──
	uploadJobs chan uploadJob // 上传任务队列，常驻 worker 消费
}

// New 创建本地同步模块实例（仅初始化状态，不启动协程）。
// 调用方：syncFile 根包的 New()。
func New(env *core.Env) *Local {
	return &Local{
		env:        env,
		pending:    make(map[string]struct{}),
		uploadJobs: make(chan uploadJob, 64),
	}
}

// Start 启动本模块的全部后台协程（都挂在传入的 wg 上，随 ctx 取消而退出）：
//  1. 启动后对主同步目录做一次全量递归同步，收敛停机期间的本地变化；
//  2. 文件监听器：监视主同步目录的实时变动，经 debounce 后触发 syncDir；
//  3. 上传 worker：从 uploadJobs 消费真正的上传任务。
func (l *Local) Start(ctx context.Context, wg *sync.WaitGroup) {
	l.FullScan(ctx) // 启动后先全量收敛一次
	wg.Go(func() { l.watchPump(ctx) })
	wg.Go(func() { l.startUploadWorkers(ctx, uploadWorkerCount) })
}
