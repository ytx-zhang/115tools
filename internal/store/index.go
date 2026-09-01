package store

import (
	"bytes"
	"context"
	"encoding/binary"

	"go.etcd.io/bbolt"
)

// Kind 索引记录的种类：本地这一项对应云端的什么。
type Kind uint8

const (
	// KindDir 云端目录。
	KindDir Kind = iota
	// KindFile 云端实体文件，本地也是实体文件（普通文件，或关闭 STRM 时的视频）。
	KindFile
	// KindStrm 云端视频，本地是 .strm 指针文件。
	KindStrm
)

// String 返回种类的可读名称（供日志与预演面板展示）。
func (k Kind) String() string {
	switch k {
	case KindDir:
		return "目录"
	case KindFile:
		return "文件"
	case KindStrm:
		return "STRM"
	default:
		return "未知"
	}
}

// Record 一条「本地路径 ↔ 云端」的同步记录。
//
// 关键设计：PickCode 显式落库。.strm 的一致性判定因此从「比本地 mtime」改为「比 pickcode」，
// 彻底消除「DB mtime ≠ 文件 mtime 就误判变更、删旧视频重传」这一类问题。
type Record struct {
	Fid      string // 云端 FID
	PickCode string // 云端 pickcode（KindStrm 时必填；其余为空）
	Kind     Kind
	Size     int64 // 云端文件字节数（仅 KindFile 有意义，其余为 0）
}

// Child 某目录直属子项的一条快照。
type Child struct {
	Name string // 相对父目录的名称（不含分隔符）
	Rec  Record
}

// Index 是同步比对需要的索引能力。窄接口，便于 plan 的单测用内存 fake 替换。
type Index interface {
	// Get 读取一条记录；不存在返回 false。
	Get(ctx context.Context, path string) (Record, bool)
	// Put 写入一条记录。
	Put(ctx context.Context, path string, r Record)
	// Children 返回 dir 的**直属**子项快照（不含更深层级）。
	Children(ctx context.Context, dir string) []Child
	// CountRecursive 递归统计 path 下的后代条目数（**不含 path 自身**，与云端目录
	// FileCount+FolderCount 口径一致，用于判断云端目录是否已完整索引）。
	CountRecursive(ctx context.Context, path string) int64
	// ListStrmFids 递归返回 dirPath 下所有 .strm 记录对应的云端 FID（删目录前先把视频搬走）。
	ListStrmFids(ctx context.Context, dirPath string) []string
	// ClearTree 删除给定路径（含全部后代）的记录。
	ClearTree(ctx context.Context, paths ...string)
}

// ──── 值编解码 ────
//
// 布局：ver(1B) | kind(1B) | size(8B BE) | fidLen(2B BE) | fid | pickCode(余下全部)
//
// ver 用于将来换格式时区分；解码遇到未知版本视为无记录（调用方按「未索引」处理）。

const recordVersion byte = 0x02

const recordHeaderLen = 1 + 1 + 8 + 2

func encodeRecord(r Record) []byte {
	buf := make([]byte, recordHeaderLen+len(r.Fid)+len(r.PickCode))
	buf[0] = recordVersion
	buf[1] = byte(r.Kind)
	binary.BigEndian.PutUint64(buf[2:10], uint64(r.Size))
	binary.BigEndian.PutUint16(buf[10:12], uint16(len(r.Fid)))
	copy(buf[12:], r.Fid)
	copy(buf[12+len(r.Fid):], r.PickCode)
	return buf
}

func decodeRecord(v []byte) (Record, bool) {
	if len(v) < recordHeaderLen || v[0] != recordVersion {
		return Record{}, false
	}
	fidLen := int(binary.BigEndian.Uint16(v[10:12]))
	if len(v) < recordHeaderLen+fidLen {
		return Record{}, false
	}
	fid := string(v[recordHeaderLen : recordHeaderLen+fidLen])
	return Record{
		Fid:      fid,
		PickCode: string(v[recordHeaderLen+fidLen:]),
		Kind:     Kind(v[1]),
		Size:     int64(binary.BigEndian.Uint64(v[2:10])),
	}, true
}

// ──── 实现 ────

// Get 读取一条记录。
func (s *Store) Get(ctx context.Context, path string) (Record, bool) {
	var rec Record
	found := false
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketIndex).Get([]byte(path)); v != nil {
			rec, found = decodeRecord(v)
		}
		return nil
	})
	if err != nil {
		logErr(ctx, "读取索引失败", err, "路径", path)
		return Record{}, false
	}
	return rec, found
}

// Fid 快捷读取 FID（无记录返回空串）。
func (s *Store) Fid(ctx context.Context, path string) string {
	rec, ok := s.Get(ctx, path)
	if !ok {
		return ""
	}
	return rec.Fid
}

// Put 写入一条记录（Batch 聚合高频写）。
func (s *Store) Put(ctx context.Context, path string, r Record) {
	if err := s.batch(ctx, func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketIndex).Put([]byte(path), encodeRecord(r))
	}); err != nil {
		logErr(ctx, "写入索引失败", err, "路径", path)
	}
}

// Children 返回 dir 的直属子项快照。单个短读事务内收集后返回，调用方据此做重活。
func (s *Store) Children(ctx context.Context, dir string) []Child {
	if ctx.Err() != nil {
		return nil
	}
	prefix := []byte(dirPrefix(dir))
	var out []Child
	err := s.view(ctx, func(tx *bbolt.Tx) error {
		return scanPrefix(ctx, tx.Bucket(bucketIndex).Cursor(), prefix, func(k, v []byte) ([]byte, error) {
			rel := k[len(prefix):]
			// 深层后代：整段跳到该子目录之后，避免无谓遍历
			if i := bytes.IndexByte(rel, '/'); i >= 0 {
				return skipSuccessor(k, len(prefix)+i), nil
			}
			rec, ok := decodeRecord(v)
			if !ok {
				return nil, nil
			}
			out = append(out, Child{Name: string(rel), Rec: rec})
			return nil, nil
		})
	})
	if err != nil {
		logErr(ctx, "扫描子项失败", err, "路径", dir)
	}
	return out
}

// CountRecursive 递归统计 path 下全部后代条目数。
func (s *Store) CountRecursive(ctx context.Context, path string) int64 {
	prefix := []byte(dirPrefix(path))
	var count int64
	if err := s.view(ctx, func(tx *bbolt.Tx) error {
		return scanPrefix(ctx, tx.Bucket(bucketIndex).Cursor(), prefix, func([]byte, []byte) ([]byte, error) {
			count++
			return nil, nil
		})
	}); err != nil {
		logErr(ctx, "统计索引失败", err, "路径", path)
	}
	return count
}

// strmSuffix 是 .strm 后缀（ListStrmFids 大小写不敏感匹配用）。
var strmSuffix = []byte(".strm")

// ListStrmFids 递归返回 dirPath 下所有 .strm 记录对应的云端 FID。
func (s *Store) ListStrmFids(ctx context.Context, dirPath string) []string {
	prefix := []byte(dirPrefix(dirPath))
	var fids []string
	if err := s.view(ctx, func(tx *bbolt.Tx) error {
		return scanPrefix(ctx, tx.Bucket(bucketIndex).Cursor(), prefix, func(k, v []byte) ([]byte, error) {
			if len(k) < len(strmSuffix) || !bytes.EqualFold(k[len(k)-len(strmSuffix):], strmSuffix) {
				return nil, nil
			}
			if rec, ok := decodeRecord(v); ok && rec.Fid != "" {
				fids = append(fids, rec.Fid)
			}
			return nil, nil
		})
	}); err != nil {
		logErr(ctx, "列出 STRM 链接失败", err, "路径", dirPath)
	}
	return fids
}

// ClearTree 删除给定路径（含全部后代）的记录，单个写事务内完成。
func (s *Store) ClearTree(ctx context.Context, paths ...string) {
	if len(paths) == 0 {
		return
	}
	err := s.update(ctx, func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketIndex)
		for _, p := range paths {
			childPrefix := []byte(dirPrefix(p))
			self := bytes.TrimSuffix(childPrefix, []byte("/"))
			c := b.Cursor()
			for k, _ := c.Seek(self); k != nil; k, _ = c.Next() {
				if !bytes.Equal(k, self) && !bytes.HasPrefix(k, childPrefix) {
					break
				}
				if err := c.Delete(); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		logErr(ctx, "批量删除索引失败", err, "数量", len(paths))
	}
}

// ──── 前缀扫描 ────

// dirPrefix 归一目录前缀为带尾斜杠形式（防 "a/b" 误匹配 "a/bc"）。
func dirPrefix(p string) string {
	if len(p) == 0 || p[len(p)-1] != '/' {
		return p + "/"
	}
	return p
}

// scanPrefix 在单个读事务内按前缀遍历，逐个 key 调用 fn（带 ctx 取消检查）。
// fn 返回非 nil 的 next 时，外层改从该位置 Seek（跳层剪枝）；否则正常 Next。
func scanPrefix(ctx context.Context, c *bbolt.Cursor, prefix []byte, fn func(k, v []byte) (next []byte, err error)) error {
	k, v := c.Seek(prefix)
	for k != nil && bytes.HasPrefix(k, prefix) {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		next, err := fn(k, v)
		if err != nil {
			return err
		}
		if next != nil {
			k, v = c.Seek(next)
		} else {
			k, v = c.Next()
		}
	}
	return nil
}

// skipSuccessor 构造 k[:n+1]（含分隔符 '/'）的严格后继，用于整段跳层。
func skipSuccessor(k []byte, n int) []byte {
	jump := make([]byte, n+2)
	copy(jump, k[:n+1])
	jump[n+1] = 0xff
	return jump
}
