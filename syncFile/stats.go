package syncFile

import (
	"sync"
	"sync/atomic"

	"github.com/tidwall/sjson"
)

type taskStats struct {
	total        atomic.Int64
	completed    atomic.Int64
	failed       atomic.Int64
	mu           sync.Mutex
	failedErrors []string
	running      atomic.Bool
}

var stats = &taskStats{
	failedErrors: []string{},
}

func (s *taskStats) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total.Store(0)
	s.completed.Store(0)
	s.failed.Store(0)
	s.failedErrors = s.failedErrors[:0]
}

func GetStatus() string {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	data, _ := sjson.Set("", "total", stats.total.Load())
	data, _ = sjson.Set(data, "completed", stats.completed.Load())
	data, _ = sjson.Set(data, "failed", stats.failed.Load())
	data, _ = sjson.Set(data, "running", stats.running.Load())
	data, _ = sjson.Set(data, "errors", stats.failedErrors)
	return data
}
func markFailed(reason string) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.failed.Add(1)
	stats.failedErrors = append(stats.failedErrors, reason)
}
