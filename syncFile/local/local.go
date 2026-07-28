// Package local 是本地同步模块（本地 → 云端上传方向）。
// 文件监听器发现变动 → 按目录静默窗口去抖 → syncDir 非递归对比直属子项 →
// 上传/建目录/清理云端多余项。与 cloud 模块不加锁，靠幂等避让。
package local

import (
	"115tools/syncFile/core"
	"context"
	"sync"
)

// Local 是本地同步模块实例。
type Local struct {
	env        *core.Env
	uploadJobs chan uploadJob // 上传任务队列，常驻 worker 消费
	inFlight   sync.Map       // 并发上传去重（key=本地路径），防同名重复上传
}

func New(env *core.Env) *Local {
	return &Local{env: env, uploadJobs: make(chan uploadJob, 64)}
}

// Start 启动后台协程（都挂 wg 上，随 ctx 取消退出）。
// ⚠️ worker 必须先于 FullScan 启动（v0.8.2 死锁坑）；
// ⚠️ FullScan 必须异步（v0.8.7：同步会阻塞 New() 返回、前端卡「重载中」）。
func (l *Local) Start(ctx context.Context, wg *sync.WaitGroup) {
	wg.Go(func() { l.startUploadWorkers(ctx, uploadWorkerCount) })
	wg.Go(func() { l.watchPump(ctx) })
	wg.Go(func() { l.FullScan(ctx) })
}
