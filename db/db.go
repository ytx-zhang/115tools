package db

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// Init 初始化数据库连接并创建 Bucket
type DB struct {
	boltDB     *bbolt.DB
	bucketName []byte
}

// New 初始化数据库实例
func New(path string) (*DB, error) {
	// 1. 打开数据库
	db, err := bbolt.Open(path, 0600, nil)
	if err != nil {
		return nil, fmt.Errorf("[数据库] 开启失败: %w", err)
	}

	// 2. 优化性能设置
	db.MaxBatchDelay = 100 * time.Millisecond

	instance := &DB{
		boltDB:     db,
		bucketName: []byte("FileIndex"),
	}

	// 3. 确保 Bucket 存在
	err = db.Update(func(tx *bbolt.Tx) error {
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
	if d.boltDB != nil {
		if err := d.boltDB.Close(); err != nil {
			slog.Error("[数据库] 关闭失败", "错误信息", err)
		} else {
			slog.Info("[数据库] 关闭成功")
		}
	}
}

// GetInfo 获取单个路径的信息
func (d *DB) GetInfo(localPath string) (fid string, size int64) {
	size = -2
	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		v := b.Get([]byte(localPath))
		if v == nil {
			return nil
		}

		if before, after, ok := bytes.Cut(v, []byte("|")); ok {
			fid = string(before)
			size, _ = strconv.ParseInt(string(after), 10, 64)
		}
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
	val := fid + "|" + strconv.FormatInt(size, 10)
	d.boltDB.Batch(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		return b.Put([]byte(localPath), []byte(val))
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
				idx := bytes.LastIndexByte(val, '|')
				isDir := false
				if idx != -1 {
					sizeBytes := val[idx+1:]
					if len(sizeBytes) == 2 && sizeBytes[0] == '-' && sizeBytes[1] == '1' {
						isDir = true
					}
				}
				if !isDir {
					if err := b.Delete(selfBytes); err != nil {
						return err
					}
					continue
				}
			}
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
		}
		return nil
	})

	if err != nil {
		slog.Error("[数据库] 批量清理失败", "数量", len(fPaths), "错误信息", err)
	}
}

// GetTotalCount 统计子项总数 (仅限统计，不建议在高频逻辑中使用)
func (d *DB) GetTotalCount(parentPath string) (count int64) {
	prefix := parentPath
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	prefixBytes := []byte(prefix)
	d.boltDB.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(d.bucketName)
		if b == nil {
			return nil
		}
		cursor := b.Cursor()
		for k, _ := cursor.Seek(prefixBytes); k != nil && bytes.HasPrefix(k, prefixBytes); k, _ = cursor.Next() {
			count++
		}
		return nil
	})
	return
}

func (d *DB) ScanChildren(ctx context.Context, workPath string, handler func(name string, valStr string)) {
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
		// 初始定位到前缀起始位置
		k, v := c.Seek(prefixBytes)
		for k != nil && bytes.HasPrefix(k, prefixBytes) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}

			// 计算相对路径的字节切片（零内存分配）
			relBytes := k[len(prefixBytes):]

			// 查找是否存在子目录分隔符 '/' (byte 47)
			if slashIdx := bytes.IndexByte(relBytes, '/'); slashIdx != -1 {
				// 发现非直属子条目（属于更深层目录）
				// 计算该子目录的完整字节前缀：父目录 + 第一层子目录名 + '/'
				subDirPrefixLen := len(prefixBytes) + slashIdx + 1
				subDirPrefix := k[:subDirPrefixLen]

				// 构造跳转目标：subDirPrefix + 0xFF
				// 这里必须 copy 一份，因为 k 的内存由 bbolt 事务管理，不可直接修改
				jumpTarget := make([]byte, len(subDirPrefix)+1)
				copy(jumpTarget, subDirPrefix)
				jumpTarget[len(subDirPrefix)] = 0xff

				// 性能飞跃点：直接跳过子目录内所有文件
				k, v = c.Seek(jumpTarget)
				continue
			}

			// 如果是直属条目，则转换为 handler 需要的字符串
			handler(string(relBytes), string(v))

			// 继续看下一个条目
			k, v = c.Next()
		}
		return nil
	})
}
