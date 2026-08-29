// Package vault 提供本地路径 ↔ 云端 FID 的索引存储（bbolt 封装）。
//
// 路径为主键，值为 (fid, size) 二进制编码（1 字节标记 + 8 字节大端 size + fid 字符串）。
// 提供前缀扫描（子项/递归计数/孤儿检测/strm FID 枚举）与 Compact 压缩。
//
// 设计约束：
//   - 读事务内禁止写库：Children 在单个短读事务内收集快照后返回，调用方再做事；
//   - 前缀扫描统一带尾斜杠，避免 "a/b" 误匹配 "a/bc"；
//   - 错误仅记录日志（journal），调用方以 SizeNotFound 区分「未找到」。
package vault

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/journal"
	"go.etcd.io/bbolt"
)

// size 字段的特殊取值。
const (
	SizeDir      int64 = -1 // 该记录对应目录
	SizeNotFound int64 = -2 // 未找到记录
)

// 值编码标记字节。
const valueTag byte = 0x01

// Child 某目录直属子项的一条快照。
type Child struct {
	Name string
	Fid  string
	Size int64
}

// Index 封装 bbolt 连接与 bucket。
type Index struct {
	db     *bbolt.DB
	bucket []byte
	path   string
	mu     sync.Mutex // 保护 Compact 期间的连接替换
}

// New 打开索引库（不存在则创建）。
func New(path string) (*Index, error) {
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("开启索引库失败: %w", err)
	}
	ix := &Index{db: db, bucket: []byte("index"), path: path}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(ix.bucket)
		return err
	}); err != nil {
		if cerr := db.Close(); cerr != nil {
			journal.Error(context.Background(), "初始化失败关闭索引库出错", "错误", cerr)
		}
		return nil, fmt.Errorf("创建索引 bucket 失败: %w", err)
	}
	return ix, nil
}

// Close 关闭索引库。
func (ix *Index) Close() {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.db != nil {
		if err := ix.db.Close(); err != nil {
			journal.Error(context.Background(), "关闭索引库失败", "错误", err)
		}
	}
}

// ──── 单条读写 ────

// Get 读取路径的 (fid, size)；未找到时 size = SizeNotFound。
func (ix *Index) Get(ctx context.Context, path string) (fid string, size int64) {
	size = SizeNotFound
	if err := ix.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(ix.bucket).Get([]byte(path))
		if v == nil {
			return nil
		}
		fid, size, _ = decodeValue(v)
		return nil
	}); err != nil {
		journal.Error(ctx, "读取索引失败", "路径", path, "错误", err)
	}
	return fid, size
}

// GetFid 快捷读取 FID。
func (ix *Index) GetFid(ctx context.Context, path string) string {
	fid, _ := ix.Get(ctx, path)
	return fid
}

// Put 写入单条记录。用 bbolt Batch 聚合高频写（≤10ms 窗口合并 commit）。
func (ix *Index) Put(ctx context.Context, path, fid string, size int64) {
	val := encodeValue(fid, size)
	if err := ix.db.Batch(func(tx *bbolt.Tx) error {
		return tx.Bucket(ix.bucket).Put([]byte(path), val)
	}); err != nil {
		journal.Error(ctx, "写入索引失败", "路径", path, "错误", err)
	}
}

// ──── 批量删除 ────

// ClearPaths 删除传入路径（含目录的全部后代）。
func (ix *Index) ClearPaths(ctx context.Context, paths []string) {
	if len(paths) == 0 {
		return
	}
	t0 := time.Now()
	if err := ix.db.Update(func(tx *bbolt.Tx) error {
		for _, p := range paths {
			if e := ix.deleteTree(tx, p); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		journal.Error(ctx, "批量删除索引失败", "数量", len(paths), "错误", err)
		return
	}
	journal.Debug(ctx, "删除索引", "路径", strings.Join(paths, ","), "耗时", time.Since(t0))
}

// deleteTree 在单个写事务内删除前缀为 prefix 的全部记录（含自身与后代）。
func (ix *Index) deleteTree(tx *bbolt.Tx, prefix string) error {
	b := tx.Bucket(ix.bucket)
	self := []byte(prefix)
	childPrefix := []byte(prefix + "/")
	c := b.Cursor()
	for k, _ := c.Seek(self); k != nil; k, _ = c.Next() {
		if !bytes.Equal(k, self) && !bytes.HasPrefix(k, childPrefix) {
			break
		}
		if err := c.Delete(); err != nil {
			return err
		}
	}
	return nil
}

// dirPrefix 归一目录前缀为带尾斜杠形式（前缀扫描统一入口，防误匹配兄弟 key）。
func dirPrefix(p string) string {
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ──── 前缀扫描 ────

// Children 读取 workPath 的直属子项快照。单个短读事务内收集后返回，调用方据此做重活。
func (ix *Index) Children(ctx context.Context, workPath string) []Child {
	if err := context.Cause(ctx); err != nil {
		return nil
	}
	prefix := dirPrefix(workPath)
	prefixBytes := []byte(prefix)
	var out []Child
	if err := ix.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(ix.bucket)
		c := b.Cursor()
		k, v := c.Seek(prefixBytes)
		for k != nil && bytes.HasPrefix(k, prefixBytes) {
			select {
			case <-ctx.Done():
				return context.Cause(ctx)
			default:
			}
			rel := k[len(prefixBytes):]
			// 深层子目录：跳到其后继，避免无谓遍历
			if i := bytes.IndexByte(rel, '/'); i != -1 {
				jump := make([]byte, len(prefixBytes)+i+2)
				copy(jump, k[:len(prefixBytes)+i+1])
				jump[len(jump)-1] = 0xff
				k, v = c.Seek(jump)
				continue
			}
			fid, size, _ := decodeValue(v)
			out = append(out, Child{Name: string(rel), Fid: fid, Size: size})
			k, v = c.Next()
		}
		return nil
	}); err != nil {
		journal.Error(ctx, "扫描子项失败", "路径", workPath, "错误", err)
	}
	return out
}

// CountRecursive 递归统计 path 下全部后代条目数（用于判断云端目录是否已完整索引）。
func (ix *Index) CountRecursive(ctx context.Context, path string) int64 {
	prefixBytes := []byte(dirPrefix(path))
	var count int64
	if err := ix.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(ix.bucket)
		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			count++
		}
		return nil
	}); err != nil {
		journal.Error(ctx, "统计索引失败", "路径", path, "错误", err)
	}
	return count
}

// FindOrphanSubdirs 返回 currentPath 下「子项仍在但目录条目已丢失」的子目录完整路径。
// 用于本地全量扫描后清理本地已删目录残留的深层记录。
func (ix *Index) FindOrphanSubdirs(ctx context.Context, currentPath string) []string {
	prefix := dirPrefix(currentPath)
	prefixBytes := []byte(prefix)
	var orphans []string
	seen := make(map[string]struct{})
	if err := ix.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(ix.bucket)
		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			rel := string(k[len(prefixBytes):])
			before, _, ok := strings.Cut(rel, "/")
			if !ok {
				continue
			}
			if _, dup := seen[before]; dup {
				continue
			}
			seen[before] = struct{}{}
			full := prefix + before
			if b.Get([]byte(full)) == nil {
				orphans = append(orphans, full)
			}
		}
		return nil
	}); err != nil {
		journal.Error(ctx, "查找孤儿子目录失败", "路径", currentPath, "错误", err)
	}
	return orphans
}

// ListStrmFids 递归返回 dirPath 下所有 .strm 文件对应的云端 FID（用于删除目录前先搬视频）。
func (ix *Index) ListStrmFids(ctx context.Context, dirPath string) []string {
	prefixBytes := []byte(dirPrefix(dirPath))
	var fids []string
	if err := ix.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(ix.bucket)
		c := b.Cursor()
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			if !strings.HasSuffix(strings.ToLower(string(k)), ".strm") {
				continue
			}
			if fid, _, ok := decodeValue(v); ok && fid != "" {
				fids = append(fids, fid)
			}
		}
		return nil
	}); err != nil {
		journal.Error(ctx, "列出 STRM 链接失败", "路径", dirPath, "错误", err)
	}
	return fids
}

// ──── 压缩 ────

// Compact 把索引库压缩到最小体积。期间全程持锁，先关主连接再操作文件。
func (ix *Index) Compact(ctx context.Context) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()

	if err := ix.db.Close(); err != nil {
		return fmt.Errorf("压缩时关闭索引库失败: %w", err)
	}
	if err := ix.compactFiles(); err != nil {
		return ix.reopenOnError("压缩失败", err)
	}
	if err := ix.reopen(); err != nil {
		return fmt.Errorf("压缩后重新打开失败: %w", err)
	}
	journal.Debug(ctx, "索引库压缩完成", "路径", ix.path)
	return nil
}

// compactFiles 执行 bbolt.Compact 的文件级压缩。
func (ix *Index) compactFiles() error {
	src, err := bbolt.Open(ix.path, 0o400, &bbolt.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); cerr != nil {
			journal.Debug(context.Background(), "压缩关闭源文件出错", "错误", cerr)
		}
	}()

	tmp := ix.path + ".compact.tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	dst, err := bbolt.Open(tmp, 0o600, nil)
	if err != nil {
		return err
	}
	if err := bbolt.Compact(dst, src, 0); err != nil {
		if cerr := dst.Close(); cerr != nil {
			journal.Debug(context.Background(), "压缩失败关闭目标文件出错", "错误", cerr)
		}
		if rerr := os.Remove(tmp); rerr != nil && !os.IsNotExist(rerr) {
			journal.Debug(context.Background(), "压缩失败清理临时文件出错", "错误", rerr)
		}
		return err
	}
	if err := dst.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, ix.path)
}

func (ix *Index) reopen() error {
	db, err := bbolt.Open(ix.path, 0o600, nil)
	if err != nil {
		return err
	}
	ix.db = db
	return nil
}

func (ix *Index) reopenOnError(format string, err error) error {
	if rerr := ix.reopen(); rerr != nil {
		return fmt.Errorf("%s: %w（恢复连接也失败: %v）", format, err, rerr)
	}
	return fmt.Errorf("%s: %w", format, err)
}

// ──── 值编解码 ────

func encodeValue(fid string, size int64) []byte {
	buf := make([]byte, 1+8+len(fid))
	buf[0] = valueTag
	binary.BigEndian.PutUint64(buf[1:9], uint64(size))
	copy(buf[9:], fid)
	return buf
}

func decodeValue(v []byte) (fid string, size int64, ok bool) {
	if len(v) < 9 || v[0] != valueTag {
		return "", 0, false
	}
	return string(v[9:]), int64(binary.BigEndian.Uint64(v[1:9])), true
}
