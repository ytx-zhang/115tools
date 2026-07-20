package db

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// SizeDir / SizeNotFound 表示数据库记录中 size 字段的特殊取值。
const (
	SizeDir      int64 = -1 // 该记录对应一个目录
	SizeNotFound int64 = -2 // 未找到记录
)

// 值编码格式：1 字节标记(0x01) + 8 字节 big-endian size + fid 字符串
const valueTagBinary byte = 0x01

// DB 封装 bbolt 数据库连接与 Bucket。
type DB struct {
	boltDB     *bbolt.DB
	bucketName []byte
	path       string   // 数据库文件路径，用于 Compact
	mu         sync.Mutex // 保护 Compact 期间的 boltDB 替换
}

// New 初始化数据库实例。
func New(path string) (*DB, error) {
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("[数据库] 开启失败: %w", err)
	}

	// 优化性能设置
	db.MaxBatchDelay = 100 * time.Millisecond

	instance := &DB{
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
		return nil, fmt.Errorf("[数据库] 创建 Bucket 失败: %w", err)
	}

	slog.Info("[数据库] 初始化成功", "路径", path)
	return instance, nil
}

// Close 关闭数据库
func (d *DB) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.boltDB != nil {
		if err := d.boltDB.Close(); err != nil {
			slog.Error("[数据库] 关闭失败", "错误信息", err)
		} else {
			slog.Info("[数据库] 关闭成功")
		}
	}
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

// GetInfo 获取单个路径的信息
func (d *DB) GetInfo(localPath string) (fid string, size int64) {
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

// GetFid 快捷获取文件 FID
func (d *DB) GetFid(localPath string) (fid string) {
	fid, _ = d.GetInfo(localPath)
	return
}

// SaveRecord 使用 Batch 批量保存记录
func (d *DB) SaveRecord(localPath string, fid string, size int64) {
	val := encodeValue(fid, size)
	d.boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		return b.Put([]byte(localPath), val)
	})
}

func (d *DB) BatchClearPaths(fPaths []string) {
	if len(fPaths) == 0 {
		return
	}
	err := d.boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}
		for _, fPath := range fPaths {
			selfBytes := []byte(fPath)
			val := b.Get(selfBytes)
			if val != nil {
				_, size, _ := decodeValue(val)
				if size == SizeDir {
					// 目录：仅删除其自身与所有子条目
					c := b.Cursor()
					childPrefix := make([]byte, len(selfBytes)+1)
					copy(childPrefix, selfBytes)
					childPrefix[len(selfBytes)] = '/'
					for k, _ := c.Seek(selfBytes); k != nil; {
						if bytes.Equal(k, selfBytes) || bytes.HasPrefix(k, childPrefix) {
							if err := c.Delete(); err != nil {
								return err
							}
							k, _ = c.Seek(k)
						} else {
							break
						}
					}
					continue
				}
			}
			// 非目录文件：直接删除自身
			if err := b.Delete(selfBytes); err != nil {
				return err
			}
		}
		return nil
	})

	if err != nil {
		slog.Error("[数据库] 批量清理失败", "数量", len(fPaths), "错误信息", err)
	}
}

// CountRecursive 递归统计 path 下所有子条目的总数（含文件和子目录）。
// 通过 bbolt 前缀扫描实现，数百万条目录毫秒级完成，远比 GetFileList API 调用便宜。
func (d *DB) CountRecursive(path string) int64 {
	prefix := path + "/"
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

// ScanChildren 遍历 workPath 的直属子条目，对每个子条目回调 (name, fid, size)。
// 通过 0xFF 跳转直接跳过深层子目录，避免无谓遍历。
func (d *DB) ScanChildren(ctx context.Context, workPath string, handler func(name string, fid string, size int64)) {
	if err := ctx.Err(); err != nil {
		return
	}
	prefix := workPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefixBytes := []byte(prefix)

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
			handler(string(relBytes), fid, size)

			k, v = c.Next()
		}
		return nil
	})
}

// Compact 将数据库压缩到最小体积，回收删除/重写产生的空洞页。
// 压缩期间 d.mu 全程持锁，先关闭主连接再操作文件，避免 Windows 文件锁冲突。
func (d *DB) Compact() error {
	if d.path == "" {
		return fmt.Errorf("[数据库] 未设置文件路径，无法压缩")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	beforeSize := fileSize(d.path)

	// 关闭主连接，释放文件锁
	if err := d.boltDB.Close(); err != nil {
		return fmt.Errorf("[数据库] 压缩时关闭 DB 失败: %w", err)
	}

	src, err := bbolt.Open(d.path, 0400, &bbolt.Options{ReadOnly: true})
	if err != nil {
		d.boltDB, _ = bbolt.Open(d.path, 0600, nil)
		return fmt.Errorf("[数据库] 压缩时打开源文件失败: %w", err)
	}
	defer src.Close()

	tmpPath := d.path + ".compact.tmp"
	os.Remove(tmpPath)

	dst, err := bbolt.Open(tmpPath, 0600, nil)
	if err != nil {
		d.boltDB, _ = reopen(d.path)
		return fmt.Errorf("[数据库] 创建压缩目标文件失败: %w", err)
	}

	if err := bbolt.Compact(dst, src, 0); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		d.boltDB, _ = reopen(d.path)
		return fmt.Errorf("[数据库] 压缩写入失败: %w", err)
	}
	dst.Close()

	if err := os.Rename(tmpPath, d.path); err != nil {
		d.boltDB, _ = reopen(d.path)
		return fmt.Errorf("[数据库] 压缩后替换文件失败，已恢复原 DB: %w", err)
	}

	d.boltDB, err = reopen(d.path)
	if err != nil {
		return fmt.Errorf("[数据库] 压缩后重新打开失败: %w", err)
	}

	afterSize := fileSize(d.path)
	slog.Info("[数据库] 压缩完成",
		"原大小", formatSize(beforeSize),
		"新大小", formatSize(afterSize),
		"释放", formatSize(beforeSize-afterSize))
	return nil
}

// reopen 打开数据库文件并设置默认配置。
func reopen(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, err
	}
	db.MaxBatchDelay = 100 * time.Millisecond
	return db, nil
}

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
