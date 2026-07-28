// Package local 是「本地同步」模块：负责把本地文件的变化同步到 115 云端
// （本地 → 云端 的上传方向）。
//
// 【数据流】
//  1. 文件监听器（watcher.go 的 watchPump）发现本地文件变动，
//     只把事件对应的「父目录」登记进待处理表（内存 map）；
//  2. 事件不是立即处理，而是按目录各自独立的静默窗口：
//     目录自身 Debounce 秒内无新事件才处理，避免「文件还在拷贝中就开始上传」；
//  3. 静默到期：对每个「云端已存在」的父目录跑一遍 syncDir（scan.go）非递归同步，
//     只处理该目录自身的直属子项；子目录各自会由自己的静默窗口独立触发；
//     syncDir 是唯一的「处理核心」，与全量扫描/定时扫描共用，保证行为完全一致；
//  4. 需要上传的文件投递到 uploadJobs 队列（upload.go），
//     由常驻上传 worker 并发执行真正的上传；
//  5. 需要在云端建目录/删文件的操作走 cloudops.go。
//
// 【极简事件处理】
// 监听器完全「不懂」业务逻辑：无视事件类型（连 rename/move 也走 syncDir），
// 只记父目录。每个目录只处理自身直接子项（syncDir 非递归），子目录各自会由
// 自己的事件触发；云端还不存在的父目录由 AddCloudFolder 从根逐级补建缺失祖先后再同步。
//
// 【与其他模块的边界】
//   - 本模块只认「本地 → 云端」方向；「云端 → 本地」是 cloud 模块的事；
//   - 共享设施来自 core.Env；与 cloud 模块不加锁互斥，靠幂等避让。
package local

import (
	"115tools/syncFile/core"
	"context"
	"sync"
)

// Local 是本地同步模块的实例，持有本模块的全部运行时状态。
// 通过 New 构造、Start 启动；后台协程随 ctx 取消而全部退出。
type Local struct {
	env *core.Env // 共享运行环境（API/DB/路径配置，见 core 包）

	// ── 上传任务队列（upload.go）──
	uploadJobs chan uploadJob // 上传任务队列，常驻 worker 消费

	// inFlight 并发上传去重：key=本地路径，value=struct{}，上传期间占位。
	// 替换场景下同名视频不再因 .strm 已存在而跳过，靠它防止同一路径的重复任务
	// 被多个 worker 同时上传产生云端副本。sync.Map 零值可用，无需在 New 中初始化。
	inFlight sync.Map
}

// New 创建本地同步模块实例（仅初始化状态，不启动协程）。
// 调用方：syncFile 根包的 New()。
func New(env *core.Env) *Local {
	return &Local{
		env:        env,
		uploadJobs: make(chan uploadJob, 64),
	}
}

// Start 启动本模块的全部后台协程（都挂在传入的 wg 上，随 ctx 取消而退出）：
//  1. 启动后对主同步目录做一次全量递归同步，收敛停机期间的本地变化；
//  2. 文件监听器：监视主同步目录的实时变动，经按目录静默窗口后触发 syncDir；
//  3. 上传 worker：从 uploadJobs 消费真正的上传任务。
func (l *Local) Start(ctx context.Context, wg *sync.WaitGroup) {
	// 必须先启动上传 worker 再跑同步：FullScan 会把待上传文件投递进
	// uploadJobs（缓冲仅 64）；若 worker 尚未启动，待上传数超过缓冲时第 N 个投递会
	// 永久阻塞在 channel 发送上（v0.8.2 死锁坑）。故 worker 必须先于扫描启动，
	// watchPump 顺序无关。
	wg.Go(func() { l.startUploadWorkers(ctx, uploadWorkerCount) })
	wg.Go(func() { l.watchPump(ctx) })
	// 首启全量扫描放到后台异步跑（与定时全量兜底路径一致）：FullScan 是同步的，
	// 会把待上传文件逐个投递进 uploadJobs，待上传量大时须等待 worker 消费腾出缓冲
	// 才继续。若放在 New() 同步调用链内，会阻塞 New() 返回，导致前端在 FullScan
	// 把所有文件「投递完」之前一直 ready=false，显示「配置热重载中，同步器正在重建」
	// （实际是首启、且上传正由 worker 在后台进行）——大媒体库/重启后大量待传时尤其明显。
	// 改为后台 goroutine：New() 立即就绪，全量收敛在后台进行，不再卡前端。
	wg.Go(func() { l.FullScan(ctx) })
}
