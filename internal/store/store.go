// Package store 提供本地路径 ↔ 云端 FID 的索引存储（bbolt 封装）。
//
// 职责：
//   - 路径为主键，值为 (fid, size) 二进制编码（0x01 + 8 字节大端 size + fid 字符串）。
//   - 前缀扫描：ScanChildren（跳深层）、CountRecursive、FindOrphanSubdirs、ListStrmFids。
//   - Compact：定期压缩回收空洞页，失败时 reopenOnError 恢复原连接。
//
// ⚠️ 历史教训：
//   - 读事务内禁止写库（ScanChildren 先返回快照再让调用方做事），否则嵌套 RLock 会死锁。
//   - 错误一律 %w 包装（不带 [数据库] 前缀）。
package store

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ytx-zhang/115tools/internal/logs"
	"go.etcd.io/bbolt"
)

// SizeDir / SizeNotFound 表示数据库记录中 size 字段的特殊取值。
const (
	SizeDir      int64 = -1 // 该记录对应一个目录
	SizeNotFound int64 = -2 // 未找到记录
)

// 值编码格式：1 字节标记(0x01) + 8 字节 big-endian size + fid 字符串
const valueTagBinary byte = 0x01

// Store 封装 bbolt 数据库连接与 Bucket。
type Store struct {
	boltDB     *bbolt.DB
	bucketName []byte
	path       string     // 数据库文件路径，用于 Compact
	mu         sync.Mutex // 保护 Compact 期间的 boltDB 替换
}

// Child 是某目录直属子项的一条快照。
type Child struct {
	Name string
	Fid  string
	Size int64
}

// New 初始化数据库实例。
func New(path string) (*Store, error) {
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("开启失败: %w", err)
	}

	instance := &Store{
		boltDB:     db,
		bucketName: []byte("FileIndex"),
		path:       path,
	}

	// 确保 Bucket 存在
	err = instance.boltDB.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(instance.bucketName)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("创建 Bucket 失败: %w", err)
	}

	logs.Info(logs.ModuleDB, "初始化成功", "路径", path)
	return instance, nil
}

// Close 关闭数据库。
func (d *Store) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.boltDB != nil {
		if err := d.boltDB.Close(); err != nil {
			logs.Error(logs.ModuleDB, "关闭失败", "错误", err)
		} else {
			logs.Info(logs.ModuleDB, "关闭成功")
		}
	}
}

// reopen 重新打开数据库连接（Compact 失败恢复/重新打开共用）。
func (d *Store) reopen() error {
	db, err := bbolt.Open(d.path, 0600, nil)
	if err != nil {
		return err
	}
	d.boltDB = db
	return nil
}

// reopenOnError 在 Compact 失败路径中恢复原连接并返回包装错误（收敛 4 处重复样板）。
func (d *Store) reopenOnError(format string, err error) error {
	if rerr := d.reopen(); rerr != nil {
		return fmt.Errorf("%s: %w（恢复连接也失败: %v）", format, err, rerr)
	}
	return fmt.Errorf("%s: %w", format, err)
}

// encodeValue 将 (fid, size) 编码为二进制：1 字节标记 + 8 字节 size + fid。
func encodeValue(fid string, size int64) []byte {
	buf := make([]byte, 1+8+len(fid))
	buf[0] = valueTagBinary
	binary.BigEndian.PutUint64(buf[1:9], uint64(size))
	copy(buf[9:], fid)
	return buf
}

// decodeValue 解析二进制值。
func decodeValue(v []byte) (fid string, size int64, ok bool) {
	if len(v) < 9 || v[0] != valueTagBinary {
		return "", 0, false
	}
	size = int64(binary.BigEndian.Uint64(v[1:9]))
	fid = string(v[9:])
	return fid, size, true
}

// GetInfo 获取单个路径的信息。
func (d *Store) GetInfo(localPath string) (fid string, size int64) {
	size = SizeNotFound
	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		v := b.Get([]byte(localPath))
		if v == nil {
			return nil
		}
		fid, size, _ = decodeValue(v)
		return nil
	})
	return
}

// GetFid 快捷获取文件 FID。
func (d *Store) GetFid(localPath string) (fid string) {
	fid, _ = d.GetInfo(localPath)
	return
}

// SaveRecord 写入单条记录。
// 用 bbolt Batch 而非 Update：Batch 是「同步等待 + 自动聚合」——同一 MaxBatchDelay
// （默认 10ms）窗口内的并发调用合并为单次 commit（云端初始化/目录扫描的高频写大幅减少
// fsync），且调用方阻塞到自己的写入落盘才返回（写后立即可见，无时序问题）。
// 单条低频写入固定多等 ≤10ms（聚合窗口），相对业务秒级操作可忽略。
// bbolt Batch 语义：fn 幂等（Put 覆盖）、错误通过 trySolo 退化为 Update 仍可靠返回。
func (d *Store) SaveRecord(localPath string, fid string, size int64) {
	logs.Debug(logs.ModuleDB, "保存记录", "路径", localPath)
	val := encodeValue(fid, size)
	if err := d.boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		return b.Put([]byte(localPath), val)
	}); err != nil {
		logs.Error(logs.ModuleDB, "保存记录失败", "路径", localPath, "错误", err)
	}
}

// deleteTree 在单个写事务内删除前缀为 prefix 的全部记录（含 prefix 自身与所有后代）。
func (d *Store) deleteTree(tx *bbolt.Tx, prefix string) error {
	t0 := time.Now()
	b := tx.Bucket(d.bucketName)
	if b == nil {
		return nil
	}
	selfBytes := []byte(prefix)
	childPrefix := []byte(prefix + "/")

	c := b.Cursor()
	for k, _ := c.Seek(selfBytes); k != nil; k, _ = c.Next() {
		if !bytes.Equal(k, selfBytes) && !bytes.HasPrefix(k, childPrefix) {
			break
		}
		if err := c.Delete(); err != nil {
			return err
		}
	}
	// 单条删除索引，可能高频（批量清理时逐条）→ Debug
	logs.Debug(logs.ModuleDB, "删除索引", "路径", prefix, "耗时", time.Since(t0))
	return nil
}

// dirPrefix 规范化目录前缀为带尾斜杠的字符串，供 bbolt 前缀扫描
// （ScanChildren/CountRecursive/FindOrphanSubdirs/ListStrmFids）统一使用。
// 不带尾斜杠会让 "a/b" 误匹配 "a/bc" 等兄弟 key（历史 S1 bug）。
func dirPrefix(p string) string {
	if !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// BatchClearPaths 删除传入路径（含目录的全部子条目）。
func (d *Store) BatchClearPaths(fPaths []string) {
	if len(fPaths) == 0 {
		return
	}
	t0 := time.Now()
	err := d.boltDB.Update(func(tx *bbolt.Tx) error {
		for _, fPath := range fPaths {
			if e := d.deleteTree(tx, fPath); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		logs.Error(logs.ModuleDB, "批量删除索引失败", "数量", len(fPaths), "错误", err, "耗时", time.Since(t0))
		return
	}
	// 单个路径单独打印（含具体路径），批量才汇总一条 Info；逐条删除索引由 deleteTree Debug 体现
	if len(fPaths) == 1 {
		logs.Info(logs.ModuleDB, "删除索引", "路径", fPaths[0], "耗时", time.Since(t0))
	} else {
		logs.Info(logs.ModuleDB, "批量删除索引", "数量", len(fPaths), "耗时", time.Since(t0))
	}
}

// FindOrphanSubdirs 扫描 currentPath 下所有条目，返回「子项仍在但目录 entry 已丢失」的子目录完整路径。
// 用于检测深层孤儿：子目录被删后其目录 DB entry 未同步清理，导致子文件（Fonts.7z 等）永久残留。
func (d *Store) FindOrphanSubdirs(currentPath string) []string {
	t0 := time.Now()
	defer func() {
		// 查询操作，只在结束时打 Debug
		logs.Debug(logs.ModuleDB, "查找孤儿子目录完成", "路径", currentPath, "耗时", time.Since(t0))
	}()
	prefix := dirPrefix(currentPath)
	prefixBytes := []byte(prefix)

	var orphans []string
	seen := make(map[string]bool)

	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			rel := string(k[len(prefixBytes):])
			slashIdx := strings.IndexByte(rel, '/')
			if slashIdx == -1 {
				continue // 直属子项，不是深层
			}
			subDir := rel[:slashIdx]
			if seen[subDir] {
				continue
			}
			seen[subDir] = true

			subDirFull := prefix + subDir
			if v := b.Get([]byte(subDirFull)); v == nil {
				orphans = append(orphans, subDirFull)
			}
		}
		return nil
	})
	return orphans
}

// CountRecursive 递归统计 path 下所有子条目的总数（含文件和子目录）。
// 通过 bbolt 前缀扫描实现，数百万条目录毫秒级完成，远比 GetFileList API 调用便宜。
func (d *Store) CountRecursive(path string) int64 {
	prefix := dirPrefix(path)
	prefixBytes := []byte(prefix)
	var count int64
	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, _ := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = c.Next() {
			count++
		}
		return nil
	})
	return count
}

// ScanChildren 读取 workPath 的直属子条目，在「单个短读事务」内一次性收集后返回快照。
// 事务在返回前已关闭，调用方据此做对比/递归/写库等重活，绝不在读事务内做事——
// 否则读事务会被「持锁贯穿整棵子树递归」，运行期其它 goroutine 的 SaveRecord（写锁）
// 一旦等待，Go 的 sync.RWMutex 会让后续嵌套 RLock 阻塞以避免写饥饿 → 永久卡死。
// 通过 0xFF 跳转直接跳过深层子目录，避免无谓遍历。
func (d *Store) ScanChildren(ctx context.Context, workPath string) []Child {
	logs.Debug(logs.ModuleDB, "扫描子条目", "路径", workPath)
	if err := ctx.Err(); err != nil {
		return nil
	}
	prefix := dirPrefix(workPath)
	prefixBytes := []byte(prefix)

	var out []Child
	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}

		c := b.Cursor()
		k, v := c.Seek(prefixBytes)
		for k != nil && bytes.HasPrefix(k, prefixBytes) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			relBytes := k[len(prefixBytes):]

			// 发现非直属子条目（属于更深层目录），直接跳过其全部内容
			if slashIdx := bytes.IndexByte(relBytes, '/'); slashIdx != -1 {
				subDirPrefixLen := len(prefixBytes) + slashIdx + 1
				subDirPrefix := k[:subDirPrefixLen]

				jumpTarget := make([]byte, len(subDirPrefix)+1)
				copy(jumpTarget, subDirPrefix)
				jumpTarget[len(subDirPrefix)] = 0xff

				k, v = c.Seek(jumpTarget)
				continue
			}

			fid, size, _ := decodeValue(v)
			out = append(out, Child{Name: string(relBytes), Fid: fid, Size: size})

			k, v = c.Next()
		}
		return nil
	})
	return out
}

// ListStrmFids 递归返回 dirPath 目录下所有 .strm 文件对应的云端视频 FID。
// 用于「删除目录」时先把有价值的视频挪到临时目录，再让目录进回收站。
// 实现：bbolt 前缀扫描 dirPath/ 下全部后代 key，按 .strm 后缀过滤并解出 fid；
// 不跳过深层目录（要的是整棵子树），故不用 ScanChildren 的 0xFF 跳转。
func (d *Store) ListStrmFids(dirPath string) (fids []string) {
	t0 := time.Now()
	defer func() {
		// 查询操作，只在结束时打 Debug
		logs.Debug(logs.ModuleDB, "列出Strm链接完成", "路径", dirPath, "数量", len(fids), "耗时", time.Since(t0))
	}()
	prefix := dirPrefix(dirPath)
	prefixBytes := []byte(prefix)

	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, v = c.Next() {
			// 只看 .strm 文件（大小写不敏感），目录壳子不关心
			if !strings.HasSuffix(strings.ToLower(string(k)), ".strm") {
				continue
			}
			if fid, _, ok := decodeValue(v); ok && fid != "" {
				fids = append(fids, fid)
			}
		}
		return nil
	})
	return fids
}

// Compact 将数据库压缩到最小体积，回收删除/重写产生的空洞页。
// 压缩期间 d.mu 全程持锁，先关闭主连接再操作文件，避免 Windows 文件锁冲突。
func (d *Store) Compact() error {
	if d.path == "" {
		return fmt.Errorf("未设置文件路径，无法压缩")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	beforeSize := fileSize(d.path)

	// 关闭主连接，释放文件锁
	if err := d.boltDB.Close(); err != nil {
		return fmt.Errorf("压缩时关闭 DB 失败: %w", err)
	}

	src, err := bbolt.Open(d.path, 0400, &bbolt.Options{ReadOnly: true})
	if err != nil {
		return d.reopenOnError("压缩时打开源文件失败", err)
	}
	defer src.Close()

	tmpPath := d.path + ".compact.tmp"
	os.Remove(tmpPath)

	dst, err := bbolt.Open(tmpPath, 0600, nil)
	if err != nil {
		return d.reopenOnError("创建压缩目标文件失败", err)
	}

	if err := bbolt.Compact(dst, src, 0); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return d.reopenOnError("压缩写入失败", err)
	}
	dst.Close()

	if err := os.Rename(tmpPath, d.path); err != nil {
		return d.reopenOnError("压缩后替换文件失败，已恢复原 DB", err)
	}

	if err := d.reopen(); err != nil {
		return fmt.Errorf("压缩后重新打开失败: %w", err)
	}

	afterSize := fileSize(d.path)
	logs.Info(logs.ModuleDB, "压缩完成",
		"原大小", formatSize(beforeSize),
		"新大小", formatSize(afterSize),
		"释放", formatSize(beforeSize-afterSize))
	return nil
}

// fileSize 读取文件字节大小（Compact 日志用）。
func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

func formatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
}
