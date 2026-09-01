package store

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"time"

	"go.etcd.io/bbolt"
)

// Scope 一次执行的作用域：本地 → 云端，或云端 → 本地。
type Scope string

const (
	ScopeUpload   Scope = "upload"
	ScopeDownload Scope = "download"
)

// String 返回作用域的可读名称。
func (s Scope) String() string {
	if s == ScopeDownload {
		return "云端"
	}
	return "本地"
}

// Trigger 一次执行的触发方式。
type Trigger string

const (
	TriggerInit   Trigger = "init"
	TriggerWatch  Trigger = "watch"
	TriggerCron   Trigger = "cron"
	TriggerManual Trigger = "manual"
)

// String 返回触发方式的可读名称。
func (t Trigger) String() string {
	switch t {
	case TriggerInit:
		return "启动"
	case TriggerWatch:
		return "监听"
	case TriggerCron:
		return "定时"
	case TriggerManual:
		return "手动"
	default:
		return string(t)
	}
}

// State 一次执行的结果。
type State string

const (
	StateSuccess  State = "success"
	StateCanceled State = "canceled"
	StateFailed   State = "failed"
)

// String 返回结果的可读名称。
func (s State) String() string {
	switch s {
	case StateSuccess:
		return "成功"
	case StateCanceled:
		return "已取消"
	case StateFailed:
		return "失败"
	default:
		return string(s)
	}
}

// Stats 一次执行的计数摘要。
type Stats struct {
	Scanned       int64 `json:"scanned"`
	Uploaded      int64 `json:"uploaded"`
	Downloaded    int64 `json:"downloaded"`
	StrmGenerated int64 `json:"strm_generated"`
	Deleted       int64 `json:"deleted"`
	Failed        int64 `json:"failed"`
	// Dirs 本次执行的处理目标（最多 1 条，按触发类型）：全量扫描=任务本地根、
	// watch 目录事件=该目录、watch 单文件=文件路径、下载=云端根目录。
	// 仅实际有动作（!Empty）时填写，避免监听高频事件因非空而刷屏落库。
	Dirs []string `json:"dirs,omitempty"`
}

// Empty 是否全零（用于判断这次执行「什么也没做」；Dirs 为展示性字段，不参与判断）。
func (s Stats) Empty() bool {
	return s.Scanned == 0 && s.Uploaded == 0 && s.Downloaded == 0 &&
		s.StrmGenerated == 0 && s.Deleted == 0 && s.Failed == 0
}

// Event 一条值得看的事件：一次执行的起止、触发方式、计数与错误。
//
// 明细日志不落库——那是 docker logs 的职责。这里只保留「用户需要回看发生过什么」的信息。
type Event struct {
	Seq         uint64    `json:"-"` // 由 key 派生，不重复落盘
	Time        time.Time `json:"time"`
	TaskID      string    `json:"task_id"`
	TaskName    string    `json:"task_name"`
	Scope       Scope     `json:"scope"`
	Trigger     Trigger   `json:"trigger"`
	State       State     `json:"state"`
	Stats       Stats     `json:"stats"`
	DurationMs  int64     `json:"duration_ms"`
	Error       string    `json:"error,omitempty"`
	PlanOnly    bool      `json:"plan_only"`    // 预演（dry-run）：只算计划未执行
	OpCounts    []OpCount `json:"op_counts"`    // 预演时按动作类型统计
	SamplePaths []string  `json:"sample_paths"` // 预演时前若干条待处理路径
}

// OpCount 预演结果里某类动作的数量（供前端按类型分组展示）。
type OpCount struct {
	Op     string `json:"op"`
	Label  string `json:"label"`
	Count  int64  `json:"count"`
	Danger bool   `json:"danger"` // 删除/移入回收站等不可逆动作
}

// maxEvents 活动流保留上限（超出后淘汰最旧的）。
const maxEvents = 5000

// Append 追加一条事件（自动分配 seq 与时间），返回其 seq。
func (s *Store) Append(ctx context.Context, e Event) (uint64, error) {
	e.Time = time.Now().Truncate(time.Millisecond)

	var seq uint64
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketActivity)
		next, err := b.NextSequence()
		if err != nil {
			return err
		}
		seq = next
		raw, err := json.Marshal(e)
		if err != nil {
			return err
		}
		return b.Put(encodeSeqKey(seq), raw)
	})
	if err != nil {
		logErr(ctx, "写入事件失败", err, "任务", e.TaskName)
		return 0, err
	}
	s.prune(ctx)
	return seq, nil
}

// List 返回事件（最新在前，支持 offset 分页）。taskID 为空表示不过滤任务。
// limit<=0 回退默认 1000（maxEvents=5000，单页上限足够滚动浏览）。
func (s *Store) List(ctx context.Context, taskID string, offset, limit int) []Event {
	if limit <= 0 {
		limit = 1000
	}
	var out []Event
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketActivity).Cursor()
		skip := offset
		for k, v := c.Last(); k != nil; k, v = c.Prev() {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				continue
			}
			if taskID != "" && e.TaskID != taskID {
				continue
			}
			if skip > 0 {
				skip--
				continue
			}
			if len(out) >= limit {
				return nil
			}
			e.Seq = binary.BigEndian.Uint64(k)
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		logErr(ctx, "读取事件失败", err, "任务", taskID)
	}
	return out
}

// DeleteTask 删除某任务的全部事件（删除任务时调用）。
func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	return s.update(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketActivity)
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var e Event
			if err := json.Unmarshal(v, &e); err != nil {
				continue
			}
			if e.TaskID == taskID {
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// prune 淘汰超出上限的最旧事件。
func (s *Store) prune(ctx context.Context) {
	_ = s.update(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketActivity)
		total := int64(b.Stats().KeyN)
		if total <= maxEvents {
			return nil
		}
		c := b.Cursor()
		for i := int64(0); i < total-maxEvents; i++ {
			k, _ := c.First()
			if k == nil {
				return nil
			}
			if err := c.Delete(); err != nil {
				return err
			}
		}
		return nil
	})
}

// encodeSeqKey 把自增 seq 编成 8 字节大端 key，保证按写入顺序自然排序。
func encodeSeqKey(seq uint64) []byte {
	k := make([]byte, 8)
	binary.BigEndian.PutUint64(k, seq)
	return k
}
