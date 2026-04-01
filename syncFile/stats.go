package syncFile

import (
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
)

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

	data, err := sonic.MarshalString(snapshot)
	if err != nil {
		return "{}"
	}
	return data
}

func (s *taskStats) markFailed(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failed.Add(1)
	s.failedErrors = append(s.failedErrors, reason)
}
