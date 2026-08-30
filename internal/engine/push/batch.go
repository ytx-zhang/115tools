package push

import (
	"sync"
	"sync/atomic"
)

// UpBatch 一批上传的收口：等待本批完成 + 统计本批实际提交的上传数。
//
// count 决定本批是否计入任务进度：全量扫描批次计入（驱动前端进度）；
// 监听直传与扫描批次并发执行，计入会互相污染计数，故不计入但仍统计提交数。
type UpBatch struct {
	wg        sync.WaitGroup
	count     bool
	submitted atomic.Int64
}

// NewUpBatch 创建一批上传；count 为 true 时本批计入任务进度。
func NewUpBatch(count bool) *UpBatch { return &UpBatch{count: count} }

// add 登记一个已提交的上传（与上传协程结束时的 done 配对）。
func (b *UpBatch) add() {
	b.wg.Add(1)
	b.submitted.Add(1)
}

// done 标记本批中的一个上传完成。
func (b *UpBatch) done() { b.wg.Done() }

// Wait 等本批投递的上传全部完成。
func (b *UpBatch) Wait() { b.wg.Wait() }

// Submitted 返回本批实际提交的上传数（同文件已在传被去重的重复投递不计）。
func (b *UpBatch) Submitted() int64 { return b.submitted.Load() }
