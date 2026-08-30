package journal

import (
	"context"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"os"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// 每 run 明细日志落盘上限：超出丢弃最旧，避免一次超大执行撑爆 journal.db。
const maxRunLogs = 1000

// seqKeyLen run 键（8 字节大端 uint64）长度。
const seqKeyLen = 8

var (
	runsBucket    = []byte("runs")
	runLogsBucket = []byte("runlogs")
	metaBucket    = []byte("meta")
)

var (
	schemaKey     = []byte("schema")
	seqCounterKey = []byte("run_seq")
)

// schemaV2 库结构版本 v2：run 序号全局唯一。
// v1 的 seq 取自各任务桶自己的 NextSequence，跨任务必然重复（A、B 都有 seq=1），
// 而明细日志以 seq 为全局键 → 后结束的 run 覆盖前一个的日志，任务日志串号。
const schemaV2 = "2"

// Store 执行历史与明细日志库（独立 bbolt 文件 journal.db）。
// 运行中的 run 的明细日志驻留内存（running map），结束时整批落盘并释放。
type Store struct {
	db    *bbolt.DB
	path  string
	mu    sync.Mutex            // 保护 running map
	runs  map[uint64]*runBuffer // seq → 运行中缓冲
	seq   uint64                // 最近分配的 seq（由 bbolt NextSequence 决定，仅作日志参考）
	prune int                   // 自上次 prune 后 Begin 次数
}

// runBuffer 一次执行在运行中的内存态。
type runBuffer struct {
	taskID    string
	taskName  string
	direction Direction
	trigger   Trigger
	startedAt time.Time
	mu        sync.Mutex
	logs      []LogEntry
	seq       uint64 // 该 run 内的日志序号
}

// New 打开（或创建）journal 库，并把上次异常退出残留的 running 记录标记为失败。
func New(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("开启历史库失败: %w", err)
	}
	s := &Store{db: db, path: path, runs: make(map[uint64]*runBuffer)}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{runsBucket, runLogsBucket, metaBucket} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		closeQuiet(db)
		return nil, fmt.Errorf("初始化历史库失败: %w", err)
	}
	if err := s.applySchema(); err != nil {
		closeQuiet(db)
		return nil, err
	}
	if err := s.markInterrupted(); err != nil {
		closeQuiet(db)
		return nil, err
	}
	return s, nil
}

// closeQuiet 关闭数据库并忽略错误（仅用于初始化失败路径，此时日志系统尚未就绪）。
func closeQuiet(db *bbolt.DB) {
	if err := db.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "关闭历史库失败: %v\n", err)
	}
}

// Close 关闭历史库。
func (s *Store) Close() {
	if err := s.db.Close(); err != nil {
		// 关闭失败不影响进程退出，仅记录到终端。
		Error(context.Background(), "关闭历史库失败", "错误", err)
	}
}

// markInterrupted 把上次进程异常退出遗留的 running 记录标记为 failed。
func (s *Store) markInterrupted() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		rb := tx.Bucket(runsBucket)
		return rb.ForEach(func(k, _ []byte) error {
			tb := rb.Bucket(k)
			if tb == nil {
				return nil
			}
			return tb.ForEach(func(seqK, v []byte) error {
				var r Run
				if err := json.Unmarshal(v, &r); err != nil || r.State != StateRunning {
					return nil
				}
				r.State = StateFailed
				r.EndedAt = time.Now()
				r.DurationMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
				r.Error = "进程异常退出，本次执行中断"
				out, err := json.Marshal(r)
				if err != nil {
					return err
				}
				return tb.Put(seqK, out)
			})
		})
	})
}

// applySchema 保障库结构版本：初始化全局 seq 计数器，并在首次升级到 v2 时校正历史数据。
func (s *Store) applySchema() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		// 计数器必须始终不低于历史最大 seq，否则新 run 会撞上 v1 遗留的重复键。
		if err := syncSeqCounter(tx); err != nil {
			return err
		}
		if string(tx.Bucket(metaBucket).Get(schemaKey)) == schemaV2 {
			return nil
		}
		if err := dropAmbiguousLogs(tx); err != nil {
			return err
		}
		return tx.Bucket(metaBucket).Put(schemaKey, []byte(schemaV2))
	})
}

// dropAmbiguousLogs 删除被多个任务的 run 共用的明细日志。
// v1 下不同任务会有相同 seq，共用同一条 runlogs 记录（内容属于最后结束的那个 run），
// 归属已不可判定，直接丢弃——宁可看不到，也不能把别的任务的日志显示出来。
func dropAmbiguousLogs(tx *bbolt.Tx) error {
	rb := tx.Bucket(runsBucket)
	lb := tx.Bucket(runLogsBucket)
	claims := make(map[uint64]int)
	var dups [][]byte
	err := rb.ForEach(func(taskK, _ []byte) error {
		tb := rb.Bucket(taskK)
		if tb == nil {
			return nil
		}
		return tb.ForEach(func(k, _ []byte) error {
			if len(k) != seqKeyLen {
				return nil
			}
			seq := binary.BigEndian.Uint64(k)
			claims[seq]++
			if claims[seq] > 1 {
				dups = append(dups, append([]byte(nil), k...))
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, k := range dups {
		if err := lb.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// syncSeqCounter 把全局 seq 计数器抬升到不低于现有 run 的最大 seq。
func syncSeqCounter(tx *bbolt.Tx) error {
	rb := tx.Bucket(runsBucket)
	mb := tx.Bucket(metaBucket)
	var max uint64
	if err := rb.ForEach(func(taskK, _ []byte) error {
		tb := rb.Bucket(taskK)
		if tb == nil {
			return nil
		}
		return tb.ForEach(func(k, _ []byte) error {
			if len(k) != seqKeyLen {
				return nil
			}
			if seq := binary.BigEndian.Uint64(k); seq > max {
				max = seq
			}
			return nil
		})
	}); err != nil {
		return err
	}
	if v := mb.Get(seqCounterKey); len(v) == seqKeyLen && binary.BigEndian.Uint64(v) >= max {
		return nil
	}
	var b [seqKeyLen]byte
	binary.BigEndian.PutUint64(b[:], max)
	return mb.Put(seqCounterKey, b[:])
}

// nextSeq 在写事务内分配一个全局唯一的 run 序号（计数器存于 meta 桶）。
func nextSeq(tx *bbolt.Tx) (uint64, error) {
	mb := tx.Bucket(metaBucket)
	var n uint64
	if v := mb.Get(seqCounterKey); len(v) == seqKeyLen {
		n = binary.BigEndian.Uint64(v)
	}
	n++
	var b [seqKeyLen]byte
	binary.BigEndian.PutUint64(b[:], n)
	return n, mb.Put(seqCounterKey, b[:])
}

// Begin 开始一次执行：分配全局唯一 seq、落一条 running 记录、建立内存日志缓冲。返回 seq。
func (s *Store) Begin(r Run) (uint64, error) {
	var seq uint64
	err := s.db.Update(func(tx *bbolt.Tx) error {
		id, err := nextSeq(tx)
		if err != nil {
			return err
		}
		rb := tx.Bucket(runsBucket)
		tb, err := rb.CreateBucketIfNotExists([]byte(r.TaskID))
		if err != nil {
			return err
		}
		seq = id
		r.Seq = id
		r.State = StateRunning
		r.StartedAt = time.Now()
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return tb.Put(seqKey(id), out)
	})
	if err != nil {
		return 0, fmt.Errorf("写入执行记录失败: %w", err)
	}

	s.mu.Lock()
	s.runs[seq] = &runBuffer{
		taskID:    r.TaskID,
		taskName:  r.TaskName,
		direction: r.Direction,
		trigger:   r.Trigger,
		startedAt: r.StartedAt,
	}
	s.seq = seq
	s.prune++
	s.mu.Unlock()

	s.maybePrune()
	return seq, nil
}

// Finish 结束一次执行：整批落盘明细日志、更新终态记录、释放内存缓冲。
func (s *Store) Finish(seq uint64, state State, c Counters, errMsg string) error {
	s.mu.Lock()
	buf := s.runs[seq]
	delete(s.runs, seq)
	s.mu.Unlock()
	if buf == nil {
		return fmt.Errorf("执行记录不存在: %d", seq)
	}

	buf.mu.Lock()
	logs := make([]LogEntry, len(buf.logs))
	copy(logs, buf.logs)
	buf.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		// 落盘明细日志
		if len(logs) > 0 {
			out, err := json.Marshal(logs)
			if err != nil {
				return err
			}
			if err := tx.Bucket(runLogsBucket).Put(seqKey(seq), out); err != nil {
				return err
			}
		}
		// 更新终态
		rb := tx.Bucket(runsBucket)
		tb := rb.Bucket([]byte(buf.taskID))
		if tb == nil {
			return nil
		}
		raw := tb.Get(seqKey(seq))
		if raw == nil {
			return nil
		}
		var r Run
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		r.State = state
		r.EndedAt = time.Now()
		r.DurationMs = r.EndedAt.Sub(buf.startedAt).Milliseconds()
		r.Counters = c
		r.Error = errMsg
		out, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return tb.Put(seqKey(seq), out)
	})
}

// AppendLog 向运行中的 run 追加一条明细日志。
// taskID 用于校验归属：无对应 run 或 run 属于别的任务时静默忽略
// （例如 run 已结束后迟到的日志，或 seq 意外撞号）。
func (s *Store) AppendLog(taskID string, seq uint64, level, msg, attrs string) {
	s.mu.Lock()
	buf := s.runs[seq]
	s.mu.Unlock()
	if buf == nil || buf.taskID != taskID {
		return
	}
	buf.mu.Lock()
	defer buf.mu.Unlock()
	if len(buf.logs) >= maxRunLogs {
		buf.logs = buf.logs[1:]
	}
	buf.seq++
	buf.logs = append(buf.logs, LogEntry{Seq: buf.seq, Time: time.Now(), Level: level, Msg: msg, Attrs: attrs})
}

// List 返回某任务最近 limit 条执行记录（倒序：最新在前）。
func (s *Store) List(taskID string, limit int) ([]Run, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	var out []Run
	err := s.db.View(func(tx *bbolt.Tx) error {
		rb := tx.Bucket(runsBucket)
		tb := rb.Bucket([]byte(taskID))
		if tb == nil {
			return nil
		}
		c := tb.Cursor()
		for k, v := c.Last(); k != nil && len(out) < limit; k, v = c.Prev() {
			var r Run
			if err := json.Unmarshal(v, &r); err != nil {
				continue
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// Logs 返回某任务某次执行的明细日志：运行中从内存取，已结束从 journal.db 取。
// taskID 用于校验归属，不属于该任务的 seq 一律返回空，杜绝任务间日志串号。
func (s *Store) Logs(taskID string, seq uint64) ([]LogEntry, error) {
	s.mu.Lock()
	buf := s.runs[seq]
	s.mu.Unlock()
	if buf != nil {
		if buf.taskID != taskID {
			return nil, nil
		}
		buf.mu.Lock()
		defer buf.mu.Unlock()
		return append([]LogEntry(nil), buf.logs...), nil
	}
	var out []LogEntry
	err := s.db.View(func(tx *bbolt.Tx) error {
		rb := tx.Bucket(runsBucket)
		tb := rb.Bucket([]byte(taskID))
		if tb == nil || tb.Get(seqKey(seq)) == nil {
			return nil // 该 seq 不属于本任务
		}
		raw := tx.Bucket(runLogsBucket).Get(seqKey(seq))
		if raw == nil {
			return nil
		}
		return json.Unmarshal(raw, &out)
	})
	return out, err
}

// DeleteTask 删除某任务的全部执行记录与明细日志。
func (s *Store) DeleteTask(taskID string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		rb := tx.Bucket(runsBucket)
		tb := rb.Bucket([]byte(taskID))
		if tb != nil {
			// 先收集该任务的 seq，删除对应 runlogs
			var seqs [][]byte
			if err := tb.ForEach(func(k, _ []byte) error {
				seqs = append(seqs, append([]byte(nil), k...))
				return nil
			}); err != nil {
				return err
			}
			if err := rb.DeleteBucket([]byte(taskID)); err != nil {
				return err
			}
			lb := tx.Bucket(runLogsBucket)
			for _, k := range seqs {
				if err := lb.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// perTaskKeepRuns 每任务保留的执行记录条数上限。
const perTaskKeepRuns = 200

// maybePrune 低频清理过期记录：每任务只保留最近 perTaskKeepRuns 条，同步回收其明细日志。
func (s *Store) maybePrune() {
	s.mu.Lock()
	n := s.prune
	s.mu.Unlock()
	if n < 64 {
		return
	}
	s.mu.Lock()
	s.prune = 0
	s.mu.Unlock()

	_ = s.db.Update(func(tx *bbolt.Tx) error {
		rb := tx.Bucket(runsBucket)
		lb := tx.Bucket(runLogsBucket)
		return rb.ForEach(func(taskK, _ []byte) error {
			tb := rb.Bucket(taskK)
			if tb == nil {
				return nil
			}
			total := 0
			if err := tb.ForEach(func(_, _ []byte) error {
				total++
				return nil
			}); err != nil {
				return err
			}
			if total <= perTaskKeepRuns {
				return nil
			}
			drop := total - perTaskKeepRuns
			c := tb.Cursor()
			for k, _ := c.First(); k != nil && drop > 0; k, _ = c.Next() {
				if err := lb.Delete(append([]byte(nil), k...)); err != nil {
					return err
				}
				if err := c.Delete(); err != nil {
					return err
				}
				drop--
			}
			return nil
		})
	})
}

// seqKey 把 uint64 编码为 8 字节大端 key。
func seqKey(seq uint64) []byte {
	var b [seqKeyLen]byte
	binary.BigEndian.PutUint64(b[:], seq)
	return b[:]
}
