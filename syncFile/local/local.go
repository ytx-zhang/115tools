// Package local 是「本地同步」模块：负责把本地文件的变化同步到 115 云端
// （本地 → 云端 的上传方向）。
//
// 【数据流】
//  1. 文件监听器（watcher.go）发现本地文件变动，或定时任务要求全量巡检，
//     通过 Enqueue 把路径登记进「待处理队列」（queue.go）；
//  2. 队列不是立即处理，而是等待一个「静默窗口」（Settle）：
//     窗口期内同一文件反复变动会不断重计时，直到文件真正消停了才处理，
//     避免「文件还在拷贝中就开始上传」；
//  3. 到期路径交给 processPath（process.go）做「单路径判定」：
//     对比本地现状与数据库记录，得出 新增/删除/修改/无变化 四种结论；
//  4. 需要上传的文件投递到 uploadJobs 队列（upload.go），
//     由 3 个常驻上传 worker 并发执行真正的上传；
//  5. 需要在云端建目录/删文件的操作走 cloudops.go。
//
// 【与其他模块的边界】
//   - 本模块只认「本地 → 云端」方向；「云端 → 本地」是 cloud 模块的事；
//   - 共享设施（API 客户端、数据库、路径配置、视频判定、strm 写入）全部来自 core.Env；
//   - 与 cloud 模块不加锁互斥，靠幂等避让：云端同步是「先写文件、紧接着写数据库」，
//     待本模块处理对应事件时数据库记录已就位，判定结果为「无变化」，
//     不会把云端刚下载的文件又反向上传回去。
package local

import (
	"115tools/syncFile/core"
	"context"
	"sync"
	"time"
)

// Local 是本地同步模块的实例，持有本模块的全部运行时状态。
// 通过 New 构造、Start 启动；后台协程随 ctx 取消而全部退出。
type Local struct {
	env *core.Env // 共享运行环境（API/DB/路径配置，见 core 包）

	// ── 待处理任务队列（queue.go）──
	mu    sync.Mutex           // 保护 paths
	paths map[string]time.Time // 待处理路径 → 最后一次事件时间（静默窗口判定用）
	ch    chan struct{}        // 「有新任务」信号，唤醒队列 worker

	// ── 上传任务队列（upload.go）──
	uploadJobs chan uploadJob // 上传任务队列，3 个常驻 worker 消费
}

// New 创建本地同步模块实例（仅初始化状态，不启动协程）。
// 调用方：syncFile 根包的 New()。
func New(env *core.Env) *Local {
	return &Local{
		env:        env,
		paths:      make(map[string]time.Time),
		ch:         make(chan struct{}, 1),
		uploadJobs: make(chan uploadJob, 64),
	}
}

// Start 启动本模块的全部后台协程，并把主同步目录登记为首次全量任务。
//
// 启动三类常驻协程（都挂在传入的 wg 上，随 ctx 取消而退出）：
//  1. 队列 worker：按静默窗口消费待处理路径；
//  2. 文件监听器：监视主同步目录的实时变动；
//  3. 上传 worker ×3：从 uploadJobs 消费真正的上传任务。
func (l *Local) Start(ctx context.Context, wg *sync.WaitGroup) {
	// 程序启动后先对主同步目录做一次全量扫描，收敛停机期间的本地变化。
	l.Enqueue(l.env.Paths.SyncPath)

	wg.Go(func() { l.queueWorker(ctx) })
	wg.Go(func() { l.watch(ctx) })
	wg.Go(func() { l.startUploadWorkers(ctx, uploadWorkerCount) })
}
