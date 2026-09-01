package engine

import (
	"context"
	"sync"

	"github.com/ytx-zhang/115tools/internal/store"
)

// triggerRank 触发方式的优先级（数值大者优先）：手动 > 定时 > 监听 > 启动。
// 同一工作项被多个来源重复投递时保留更高优先级，保证手动全量扫描一定以 manual 身份留记录。
var triggerRank = map[store.Trigger]int{
	store.TriggerInit:   0,
	store.TriggerWatch:  1,
	store.TriggerCron:   2,
	store.TriggerManual: 3,
}

// Job 一份待执行的工作。
//
// File 与 Dir 二选一：File 是监听直传的单文件（立即同步），Dir 是目录扫描起点。
// 两者都空表示该任务该作用域的根目录。
type Job struct {
	TaskID  string
	Scope   store.Scope
	Trigger store.Trigger
	Dir     string
	File    string
}

// dedupKey 去重键：同一任务同一作用域同一目标，排队中只保留一份。
func (j Job) dedupKey() string {
	if j.File != "" {
		return j.TaskID + "|file|" + j.File
	}
	return j.TaskID + "|dir|" + j.Dir
}

// Queue 全局单消费者工作队列。
//
// 设计依据：上传必须全局串行（撞 115 风控）、云端 API 已被包级限流器卡死、本地扫描是廉价 I/O。
// 因此不再做「任务内并行 + 任务间并行 + push/pull 互斥」的四层并发控制，全部工作排成一列
// 一次跑一个，去重与合并由队列承担。由此 TaskUnit 时代的六把锁、四个 context、waitPullIdle
// 让路等待、包级 uploadMu 与 inFlight 全部消失。
type Queue struct {
	mu      sync.Mutex
	order   []string
	pending map[string]Job
	kick    chan struct{}
	closed  bool
}

// NewQueue 创建队列。
func NewQueue() *Queue {
	return &Queue{pending: make(map[string]Job), kick: make(chan struct{}, 1)}
}

// Enqueue 投递一份工作：已排队则合并，触发方式按优先级升级。
func (q *Queue) Enqueue(j Job) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	key := j.dedupKey()
	if prev, ok := q.pending[key]; ok {
		// 已在排队：保留优先级更高的触发方式
		if triggerRank[j.Trigger] > triggerRank[prev.Trigger] {
			prev.Trigger = j.Trigger
			q.pending[key] = prev
		}
		q.mu.Unlock()
		return
	}
	q.pending[key] = j
	q.order = append(q.order, key)
	q.mu.Unlock()

	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Take 阻塞取出下一份工作；ctx 取消或队列关闭返回 false。
func (q *Queue) Take(ctx context.Context) (Job, bool) {
	for {
		q.mu.Lock()
		if len(q.order) > 0 {
			key := q.order[0]
			q.order = q.order[1:]
			job := q.pending[key]
			delete(q.pending, key)
			q.mu.Unlock()
			return job, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return Job{}, false
		}

		select {
		case <-ctx.Done():
			return Job{}, false
		case <-q.kick:
		}
	}
}

// DropTask 丢弃某任务全部待执行工作（停止任务 / 删除任务时调用）。
func (q *Queue) DropTask(taskID string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.order[:0]
	for _, k := range q.order {
		if j := q.pending[k]; j.TaskID == taskID {
			delete(q.pending, k)
			continue
		}
		kept = append(kept, k)
	}
	q.order = kept
}

// Close 关闭队列（进程退出时调用），之后 Enqueue 被忽略、Take 返回 false。
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closed = true
	select {
	case q.kick <- struct{}{}:
	default:
	}
}

// Len 返回当前排队数（诊断用）。
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
}

// HasPending 该任务是否还有排队中的工作（前端据此展示「排队中」）。
func (q *Queue) HasPending(taskID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, j := range q.pending {
		if j.TaskID == taskID {
			return true
		}
	}
	return false
}
