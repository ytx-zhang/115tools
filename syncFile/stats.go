package syncFile

import (
	"encoding/json"
	"sync"
	"sync/atomic"
)

// maxFailedErrors 限制保留的失败原因条数，防止长任务持续失败时 slice 无界增长
// （failed 计数仍精确累加，此处仅裁剪明细列表）。
const maxFailedErrors = 100

type taskStats struct {
	total        atomic.Int64
	completed    atomic.Int64
	failed       atomic.Int64
	mu           sync.Mutex
	failedErrors []string
	running      atomic.Bool
}

type statsSnapshot struct {
	Total     int64    `json:"total"`
	Completed int64    `json:"completed"`
	Failed    int64    `json:"failed"`
	Running   bool     `json:"running"`
	Errors    []string `json:"errors"`
}

func (s *taskStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.Store(0)
	s.completed.Store(0)
	s.failed.Store(0)
	s.failedErrors = s.failedErrors[:0]
}

func (s *taskStats) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := statsSnapshot{
		Total:     s.total.Load(),
		Completed: s.completed.Load(),
		Failed:    s.failed.Load(),
		Running:   s.running.Load(),
		Errors:    s.failedErrors,
	}

	data, err := json.Marshal(snapshot)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func (s *taskStats) markFailed(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed.Add(1)
	// 超过上限时丢弃最旧的明细，保留最近 maxFailedErrors 条
	s.failedErrors = append(s.failedErrors, reason)
	if len(s.failedErrors) > maxFailedErrors {
		s.failedErrors = s.failedErrors[len(s.failedErrors)-maxFailedErrors:]
	}
}
