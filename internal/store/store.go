// Package store 是本地持久化层：单个 bbolt 库（sync.db）承载两块数据。
//
//   - index：本地路径 → 云端同步记录（Record），供同步比对。记录里显式带上 Kind 与 PickCode，
//     一致性判定不再依赖把 size 字段复用成 mtime，各分支不必各自猜测面对的是哪种语义；
//   - activity：值得看的事件流（执行起止 / 触发方式 / 计数 / 错误），供 Web 面板回看。
//
// 打日志一律走标准库 log/slog → stdout（docker 负责存储与轮转），本包不再落日志。
package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"go.etcd.io/bbolt"
)

// 两个 bucket：索引与活动流同库存放，避免为两块小数据各开一个文件。
var (
	bucketIndex    = []byte("index")
	bucketActivity = []byte("activity")
)

// Store 持久化库（索引 + 活动流）。并发安全。
type Store struct {
	db   *bbolt.DB
	path string
	mu   sync.Mutex // 保护 Compact 期间的连接替换
}

// New 打开数据库（不存在则创建），并确保两个 bucket 就绪。
func New(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("开启数据库失败: %w", err)
	}
	s := &Store{db: db, path: path}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketIndex, bucketActivity} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close() //nolint:errcheck // 已失败收尾，关闭错误无补救动作
		return nil, fmt.Errorf("初始化 bucket 失败: %w", err)
	}
	return s, nil
}

// Path 返回数据库文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭数据库。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

// Compact 把数据库压缩到最小体积。期间全程持锁：先关主连接，再对文件做一次性压缩后重开。
func (s *Store) Compact(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}

	if err := s.db.Close(); err != nil {
		return fmt.Errorf("压缩时关闭数据库失败: %w", err)
	}
	// 压缩失败也必须把连接恢复，否则整个进程后续索引读写全挂。
	if err := s.compactFiles(); err != nil {
		if rerr := s.reopen(); rerr != nil {
			return fmt.Errorf("压缩失败: %w（恢复连接也失败: %v）", err, rerr)
		}
		return fmt.Errorf("压缩失败: %w", err)
	}
	if err := s.reopen(); err != nil {
		return fmt.Errorf("压缩后重新打开失败: %w", err)
	}
	slog.DebugContext(ctx, "数据库压缩完成", "路径", s.path)
	return nil
}

// compactFiles 用只读源 + 临时目标做文件级压缩，成功后原子改名覆盖。
func (s *Store) compactFiles() error {
	src, err := bbolt.Open(s.path, 0o400, &bbolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }() //nolint:errcheck // 只读源关闭失败无需处理

	tmp := s.path + ".compact.tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	dst, err := bbolt.Open(tmp, 0o600, nil)
	if err != nil {
		return err
	}
	if err := bbolt.Compact(dst, src, 0); err != nil {
		_ = dst.Close() //nolint:errcheck // 压缩失败收尾，关闭错误无补救动作
		if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
			slog.Debug("压缩失败清理临时文件出错", "错误", rerr)
		}
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) reopen() error {
	db, err := bbolt.Open(s.path, 0o600, nil)
	if err != nil {
		return err
	}
	s.db = db
	return nil
}

// view 在只读事务内执行 fn。
func (s *Store) view(ctx context.Context, fn func(tx *bbolt.Tx) error) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return context.Cause(ctx)
	}
	return db.View(fn)
}

// update 在写事务内执行 fn。
func (s *Store) update(ctx context.Context, fn func(tx *bbolt.Tx) error) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return context.Cause(ctx)
	}
	return db.Update(fn)
}

// batch 聚合高频写（≤10ms 窗口合并 commit）。Batch 会阻塞到本批提交完成。
func (s *Store) batch(ctx context.Context, fn func(tx *bbolt.Tx) error) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return context.Cause(ctx)
	}
	return db.Batch(fn)
}

// logErr 统一记录持久化层的非致命错误（调用方继续，不中断同步）。
func logErr(ctx context.Context, msg string, err error, kv ...any) {
	slog.ErrorContext(ctx, msg, append(kv, "错误", err)...)
}
