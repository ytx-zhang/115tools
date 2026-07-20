package db

import (
	"log/slog"
	"sync"

	"go.etcd.io/bbolt"
)

// BatchWriter 缓冲多条写入，在缓冲达到阈值或显式 Flush 时，于单个事务内同步落盘。
//
// 设计要点：
//   - 无后台协程：避免进程退出时后台 goroutine 在 db.Close 之后写入的竞态。
//   - 调用方须在任务结束（defer）时调用 Flush()，确保缓冲全部落盘后再被 wg 计数结束，
//     进而保证 main 在 db.Close 之前所有写入已完成。
//   - Put 可被多个 goroutine 并发调用（内部加锁保护缓冲）。
type BatchWriter struct {
	db  *DB
	max int
	mu  sync.Mutex
	buf []record
}

type record struct {
	path string
	fid  string
	size int64
}

// NewBatchWriter 创建批量写入器，maxItems 为触发自动 flush 的缓冲阈值（<=0 时使用默认 500）。
func NewBatchWriter(db *DB, maxItems int) *BatchWriter {
	if maxItems <= 0 {
		maxItems = 500
	}
	return &BatchWriter{db: db, max: maxItems}
}

// Put 写入一条记录；若缓冲达到阈值则立即同步 flush。
func (w *BatchWriter) Put(path, fid string, size int64) {
	w.mu.Lock()
	w.buf = append(w.buf, record{path: path, fid: fid, size: size})
	if len(w.buf) >= w.max {
		w.flushLocked()
	}
	w.mu.Unlock()
}

// Flush 将缓冲中的记录一次性写入；缓冲为空时无操作。
func (w *BatchWriter) Flush() {
	w.mu.Lock()
	w.flushLocked()
	w.mu.Unlock()
}

func (w *BatchWriter) flushLocked() {
	if len(w.buf) == 0 {
		return
	}
	batch := w.buf
	w.buf = nil
	err := w.db.boltDB.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(w.db.bucketName)
		if b == nil {
			return nil
		}
		for _, r := range batch {
			if err := b.Put([]byte(r.path), encodeValue(r.fid, r.size)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		slog.Error("[数据库] 批量写入失败", "数量", len(batch), "错误信息", err)
	}
}
